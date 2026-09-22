package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/runpod"
	"github.com/sruckh/timbre/internal/voices"
	"github.com/sruckh/timbre/internal/web"
)

// queueLimit is how many of a user's jobs the queue fragment shows.
const (
	queueLimit            = 10
	maxAuKRequestBytes    = 34 << 20
	maxAuKMultipartMemory = 1 << 20
)

// maxNewTokensCeiling bounds the one generation parameter the form exposes. The
// handler defaults to 4096; a larger value only buys a longer render.
const maxNewTokensCeiling = 8192

// handleStudio renders the primary studio view at /: compose card, voice
// library, live queue and the playback spoken line in one page.
func (s *Server) handleStudio(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	items, err := s.jobs.ListForUser(r.Context(), userID, queueLimit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	available, err := s.voiceCards(r, userID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	_ = web.Studio(items, available, audioDurations(items), selectedTake(r)).Render(s.navContext(r), w)
}

// selectedTake reads the take the queue is showing in the player. The queue
// fragment sends it back on every poll, which is what keeps the highlighted row
// highlighted across a swap that replaces the whole table every two seconds.
// Zero means "no explicit selection" — the studio then falls back to the most
// recent ready take.
func selectedTake(r *http.Request) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("take")), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// handleQueuePage renders the compose form and the current queue.
func (s *Server) handleQueuePage(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	items, err := s.jobs.ListForUser(r.Context(), userID, queueLimit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	available, err := s.voices.List(r.Context(), userID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	_ = web.QueuePage(items, available, audioDurations(items), selectedTake(r)).Render(s.navContext(r), w)
}

// handleQueue answers GET /jobs with the queue fragment (or the row list, for
// an Accept: application/json caller).
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	items, err := s.jobs.ListForUser(r.Context(), userID, queueLimit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	s.renderQueue(w, r, items, 0)
}

// handleCreateJob answers POST /jobs: validate, insert a `queued` row, and
// return the refreshed queue.
//
// It deliberately does not contact RunPod. Submission belongs to the background
// worker — a browser request that waited on RunPod would blow Cloudflare's 90s
// cap on the first cold start.
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if err := parseJobForm(w, r); err != nil {
		http.Error(w, "could not read the form: "+err.Error(), http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	model, err := jobs.ResolveModel(r.PostFormValue("model"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var params map[string]any
	var inputPaths []string
	var voiceID int64
	if model == jobs.AuKModel {
		params, inputPaths, voiceID, err = s.parseAuKJobParams(r, userID)
	} else {
		params, err = parseJobParams(r, model)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	keepInputs := false
	defer func() {
		if !keepInputs {
			removePaths(inputPaths)
		}
	}()

	designRender := false
	if model == jobs.BreezeModel {
		mode, _ := params["mode"].(string)
		if mode == "" {
			http.Error(w, "select a Breeze mode: clone, design or direction", http.StatusBadRequest)
			return
		}
		if mode == runpod.BreezeModeDesign || mode == runpod.BreezeModeDirection {
			if _, ok := params["instruct"].(string); !ok {
				http.Error(w, "Breeze "+mode+" mode needs a voice instruction", http.StatusBadRequest)
				return
			}
		}
		designRender = mode == runpod.BreezeModeDesign
	}

	needsVoice := model != jobs.AuKModel && !designRender
	if needsVoice {
		voiceID, err = strconv.ParseInt(strings.TrimSpace(r.PostFormValue("voice_id")), 10, 64)
		if err != nil || voiceID <= 0 {
			http.Error(w, jobs.ErrNoVoice.Error(), http.StatusBadRequest)
			return
		}
		v, err := s.voices.Get(r.Context(), voiceID)
		if err != nil {
			if errors.Is(err, voices.ErrNotFound) {
				http.Error(w, "that voice no longer exists", http.StatusBadRequest)
				return
			}
			serverError(w, r, err)
			return
		}
		accessible, err := s.voices.IsAccessibleToUser(r.Context(), v.ID, userID)
		if err != nil {
			serverError(w, r, err)
			return
		}
		if !accessible {
			http.Error(w, "you do not have access to that voice", http.StatusForbidden)
			return
		}
	}

	text := r.PostFormValue("text")
	language := r.PostFormValue("language")
	if model == jobs.AuKModel {
		text = r.PostFormValue("instruction")
		language = ""
	}
	id, err := s.jobs.Enqueue(r.Context(), jobs.NewJob{
		UserID:   userID,
		VoiceID:  voiceID,
		Text:     text,
		Language: language,
		Model:    model,
		Params:   params,
	})
	if err != nil {
		if errors.Is(err, jobs.ErrEmptyText) || errors.Is(err, jobs.ErrTextTooLong) ||
			errors.Is(err, jobs.ErrLanguage) || errors.Is(err, jobs.ErrNoVoice) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		serverError(w, r, err)
		return
	}
	keepInputs = true

	if wantsJSON(r) {
		created, err := s.jobs.Get(r.Context(), id, userID)
		if err != nil {
			serverError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(created)
		return
	}

	items, err := s.jobs.ListForUser(r.Context(), userID, queueLimit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	s.renderQueue(w, r, items, id)
}

func parseJobForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuKRequestBytes)
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		return r.ParseMultipartForm(maxAuKMultipartMemory)
	}
	return r.ParseForm()
}

func (s *Server) parseAuKJobParams(r *http.Request, userID int64) (map[string]any, []string, int64, error) {
	params := map[string]any{}
	paths := make([]string, 0, 2)
	cleanup := func(err error) (map[string]any, []string, int64, error) {
		removePaths(paths)
		return nil, nil, 0, err
	}

	if strings.TrimSpace(r.PostFormValue("prompt_audio")) != "" {
		return cleanup(errors.New("prompt audio comes from the selected voice card"))
	}
	if file, _, err := r.FormFile("prompt_audio_file"); err == nil {
		_ = file.Close()
		return cleanup(errors.New("prompt audio comes from the selected voice card"))
	} else if !errors.Is(err, http.ErrMissingFile) && !errors.Is(err, http.ErrNotMultipart) {
		return cleanup(fmt.Errorf("read prompt_audio_file: %w", err))
	}

	audio, audioPath, err := s.parseAuKAudio(r, userID, "audio", "audio_file")
	if err != nil {
		return cleanup(err)
	}
	if audioPath != "" {
		paths = append(paths, audioPath)
	}

	var sourceJobID int64
	if raw := strings.TrimSpace(r.PostFormValue("source_job_id")); raw != "" {
		sourceJobID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || sourceJobID <= 0 {
			return cleanup(errors.New("select a valid ready render"))
		}
		if audioPath != "" {
			return cleanup(errors.New("choose either an audio upload or the selected render, not both"))
		}
		audioPath, err = s.copyAuKRender(r.Context(), userID, sourceJobID)
		if err != nil {
			return cleanup(err)
		}
		paths = append(paths, audioPath)
		params["source_job_id"] = sourceJobID
	}
	if audioPath != "" {
		params["audio_path"] = audioPath
	}

	in := runpod.AuKInput{
		Task:             strings.TrimSpace(r.PostFormValue("task")),
		Instruction:      strings.TrimSpace(r.PostFormValue("instruction")),
		Audio:            auKValidationAudio(audio, audioPath),
		PromptText:       strings.TrimSpace(r.PostFormValue("prompt_text")),
		ModelVariant:     strings.TrimSpace(r.PostFormValue("model_variant")),
		ResponseDelivery: strings.TrimSpace(r.PostFormValue("response_delivery")),
	}

	var voiceID int64
	task := in.Task
	if task == "" {
		task = runpod.AuKTaskAuto
	}
	if task == runpod.AuKTaskAuto || task == runpod.AuKTaskZeroShotTTS {
		voiceID, err = strconv.ParseInt(strings.TrimSpace(r.PostFormValue("voice_id")), 10, 64)
		if err != nil || voiceID <= 0 {
			if task == runpod.AuKTaskZeroShotTTS {
				return cleanup(errors.New("zero-shot TTS requires a selected cloned voice"))
			}
			voiceID = 0
		} else {
			voice, getErr := s.voices.Get(r.Context(), voiceID)
			if getErr != nil {
				if errors.Is(getErr, voices.ErrNotFound) {
					return cleanup(errors.New("that voice no longer exists"))
				}
				return cleanup(getErr)
			}
			accessible, accessErr := s.voices.IsAccessibleToUser(r.Context(), voice.ID, userID)
			if accessErr != nil {
				return cleanup(accessErr)
			}
			if !accessible {
				return cleanup(errors.New("you do not have access to that voice"))
			}
			if voice.Kind != voices.KindCloned {
				if task == runpod.AuKTaskZeroShotTTS {
					return cleanup(errors.New("zero-shot TTS requires a selected cloned voice with reference audio"))
				}
				voiceID = 0
			} else {
				data, _, refErr := s.voices.Reference(r.Context(), voice.ID)
				if refErr != nil {
					if errors.Is(refErr, voices.ErrNoReference) && task == runpod.AuKTaskAuto {
						voiceID = 0
					} else if errors.Is(refErr, voices.ErrNoReference) {
						return cleanup(errors.New("zero-shot TTS requires a selected cloned voice with reference audio"))
					} else {
						return cleanup(refErr)
					}
				} else {
					promptPath, saveErr := s.saveAuKInput(userID, "prompt_audio", data)
					if saveErr != nil {
						return cleanup(saveErr)
					}
					paths = append(paths, promptPath)
					params["prompt_audio_path"] = promptPath
					in.PromptAudio = auKValidationAudio("", promptPath)
					// Deliberately not auto-filled from voice.ReferenceTranscript.
					// The AuK worker has no dedicated transcript parameter — it
					// concatenates prompt_text onto the instruction text itself
					// ("\nReference audio transcript: ...", confirmed in
					// sruckh/tencent-auk's engine.py). A short reference clip
					// paired with a multi-sentence stored transcript has been
					// observed making the model echo the reference instead of
					// the target phrase. The form's override field remains
					// available for a user who wants to opt into trying one.
				}
			}
		}
	}

	if task == runpod.AuKTaskAuto && in.PromptAudio == "" {
		in.PromptText = ""
	}

	if raw := strings.TrimSpace(r.PostFormValue("gen_seconds")); raw != "" {
		in.GenSeconds, err = strconv.ParseFloat(raw, 64)
		if err != nil || in.GenSeconds < 0.5 || in.GenSeconds > 300 {
			return cleanup(errors.New("gen_seconds must be between 0.5 and 300"))
		}
	}
	if raw := strings.TrimSpace(r.PostFormValue("nfe")); raw != "" {
		in.NFE, err = strconv.Atoi(raw)
		if err != nil {
			return cleanup(errors.New("nfe must be a whole number"))
		}
	}
	if raw := strings.TrimSpace(r.PostFormValue("cfg_scale")); raw != "" {
		in.CfgScale, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			return cleanup(errors.New("cfg_scale must be a number"))
		}
	}
	if raw := strings.TrimSpace(r.PostFormValue("seed")); raw != "" {
		seed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || seed < 0 {
			return cleanup(errors.New("seed must be a non-negative number"))
		}
		in.Seed = &seed
	}

	in = runpod.NormalizeAuKInput(in)
	if err := runpod.ValidateAuKInput(in); err != nil {
		return cleanup(err)
	}
	params["task"] = in.Task
	params["model_variant"] = in.ModelVariant
	params["nfe"] = in.NFE
	params["cfg_scale"] = in.CfgScale
	params["response_delivery"] = in.ResponseDelivery
	if in.PromptText != "" {
		params["prompt_text"] = in.PromptText
	}
	if in.GenSeconds != 0 {
		params["gen_seconds"] = in.GenSeconds
	}
	if in.Seed != nil {
		params["seed"] = *in.Seed
	}
	return params, paths, voiceID, nil
}

func (s *Server) parseAuKAudio(r *http.Request, userID int64, valueField, fileField string) (string, string, error) {
	if strings.TrimSpace(r.PostFormValue(valueField)) != "" {
		return "", "", fmt.Errorf("%s must come from an upload or selected ready render", valueField)
	}
	file, _, fileErr := r.FormFile(fileField)
	// A urlencoded request has no file part at all: ErrNotMultipart means
	// "nothing uploaded here", exactly like ErrMissingFile in multipart data.
	if errors.Is(fileErr, http.ErrMissingFile) || errors.Is(fileErr, http.ErrNotMultipart) {
		return "", "", nil
	}
	if fileErr != nil {
		return "", "", fmt.Errorf("read %s: %w", fileField, fileErr)
	}
	data, err := io.ReadAll(io.LimitReader(file, runpod.AuKMaxAudioBytes+1))
	closeErr := file.Close()
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", fileField, err)
	}
	if closeErr != nil {
		return "", "", fmt.Errorf("close %s: %w", fileField, closeErr)
	}
	if len(data) == 0 {
		return "", "", fmt.Errorf("%s is empty", fileField)
	}
	if len(data) > runpod.AuKMaxAudioBytes {
		return "", "", fmt.Errorf("%s exceeds the 15 MB decoded limit", fileField)
	}
	path, err := s.saveAuKInput(userID, valueField, data)
	return "", path, err
}

func (s *Server) copyAuKRender(ctx context.Context, userID, sourceJobID int64) (string, error) {
	job, err := s.jobs.Get(ctx, sourceJobID, userID)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			return "", errors.New("selected render was not found")
		}
		return "", fmt.Errorf("load selected render: %w", err)
	}
	if job.Status != jobs.StatusReady || strings.TrimSpace(job.AudioPath) == "" {
		return "", errors.New("selected render is not ready")
	}

	file, err := os.Open(job.AudioPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("selected render audio is missing")
	}
	if err != nil {
		return "", fmt.Errorf("open selected render audio: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, runpod.AuKMaxAudioBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return "", fmt.Errorf("read selected render audio: %w", readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close selected render audio: %w", closeErr)
	}
	if len(data) == 0 {
		return "", errors.New("selected render audio is empty")
	}
	if len(data) > runpod.AuKMaxAudioBytes {
		return "", errors.New("selected render exceeds the 15 MB decoded limit")
	}
	return s.saveAuKInput(userID, "audio", data)
}

func (s *Server) saveAuKInput(userID int64, field string, data []byte) (string, error) {
	dir := filepath.Join(s.cfg.AudioDir, "inputs", "user_"+strconv.FormatInt(userID, 10))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create private AuK input directory: %w", err)
	}
	file, err := os.CreateTemp(dir, field+"-*")
	if err != nil {
		return "", fmt.Errorf("create private AuK input: %w", err)
	}
	path := file.Name()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("store private AuK input: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close private AuK input: %w", err)
	}
	return path, nil
}

func auKValidationAudio(direct, path string) string {
	if direct != "" {
		return direct
	}
	if path != "" {
		return "YQ=="
	}
	return ""
}

func removePaths(paths []string) {
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			_ = os.Remove(path)
		}
	}
}

// renderQueue writes the queue as JSON or as the HTMX fragment.
//
// The fragment is only ever the queue: the player lives outside it, so this
// two-second swap can never replace a playing <audio> element out from under
// the user.
func (s *Server) renderQueue(w http.ResponseWriter, r *http.Request, items []jobs.Job, justQueued int64) {
	if wantsJSON(r) {
		if items == nil {
			items = []jobs.Job{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = web.Queue(items, justQueued, audioDurations(items), selectedTake(r)).Render(r.Context(), w)
}

// handleJobPlayer answers GET /jobs/{id}/player with the playback fragment for
// one take — what a click on a queue row swaps into the player. It is a
// separate route precisely so selecting a take is the only thing that
// re-renders the player.
func (s *Server) handleJobPlayer(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}

	job, err := s.jobs.Get(r.Context(), id, userID)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		serverError(w, r, err)
		return
	}
	if job.UserID != userID {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	durations := audioDurations([]jobs.Job{job})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = web.PlayerBody(job, durations[job.ID]).Render(r.Context(), w)
}

// audioDurations labels ready jobs' audio length ("0:06.02") from the saved
// WAV's byte size — a 44-byte header, then 16-bit mono samples at the recorded
// rate. Jobs whose file is missing simply get no label and render a dash.
func audioDurations(items []jobs.Job) map[int64]string {
	out := map[int64]string{}
	for _, j := range items {
		if j.Status != jobs.StatusReady || j.AudioPath == "" {
			continue
		}
		info, err := os.Stat(j.AudioPath)
		if err != nil {
			continue
		}
		rate := j.SampleRate
		if rate <= 0 {
			rate = 24000
		}
		secs := float64(info.Size()-44) / float64(rate*2)
		if secs < 0 {
			continue
		}
		mins := int(secs) / 60
		out[j.ID] = fmt.Sprintf("%d:%05.2f", mins, secs-float64(mins*60))
	}
	return out
}

// parseJobParams reads the optional generation parameters into the map stored
// as params_json and merged into the RunPod input at submit time. The studio's
// parameter fields — seed, pace, pitch, expressiveness and the output toggles —
// all land here; every one is validated so a bad value answers 400 rather than
// silently reaching the endpoint.
func parseJobParams(r *http.Request, model string) (map[string]any, error) {
	params := map[string]any{}

	// Breeze-only fields, read ONLY for Breeze. The studio hides these controls
	// with x-show, which sets display:none and still submits them — so a MOSS or
	// Higgs render posts mode=clone and cfg_scale=4 whether or not anyone
	// touched them. Storing those would put them in params_json, and buildInput
	// forwards params_json to the worker as Extra, silently changing what MOSS
	// receives. Gating on the engine keeps the regression contract: a MOSS or
	// Higgs request with today's exact fields produces today's exact payload.
	if model == jobs.BreezeModel {
		if raw := strings.TrimSpace(r.PostFormValue("mode")); raw != "" {
			switch raw {
			case runpod.BreezeModeClone, runpod.BreezeModeDesign, runpod.BreezeModeDirection:
				params["mode"] = raw
			default:
				return nil, errors.New("mode must be clone, design or direction")
			}
		}

		if raw := strings.TrimSpace(r.PostFormValue("instruct")); raw != "" {
			params["instruct"] = raw
		}

		// The worker coerces cfg_scale with float() and rejects only
		// non-numerics, so there is no worker-side range to mirror here. Any
		// bound Timbre adds would be a Timbre choice presented as a worker limit.
		if raw := strings.TrimSpace(r.PostFormValue("cfg_scale")); raw != "" {
			cfg, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, errors.New("cfg_scale must be a number")
			}
			params["cfg_scale"] = cfg
		}
	}

	if raw := strings.TrimSpace(r.PostFormValue("max_new_tokens")); raw != "" {
		tokens, err := strconv.Atoi(raw)
		if err != nil || tokens < 1 || tokens > maxNewTokensCeiling {
			return nil, errors.New("max_new_tokens must be a number between 1 and " +
				strconv.Itoa(maxNewTokensCeiling))
		}
		params["max_new_tokens"] = tokens
	}

	if raw := strings.TrimSpace(r.PostFormValue("seed")); raw != "" {
		seed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || seed < 0 {
			return nil, errors.New("seed must be a non-negative number")
		}
		params["seed"] = seed
	}

	if raw := strings.TrimSpace(r.PostFormValue("pace")); raw != "" {
		pace, err := strconv.ParseFloat(raw, 64)
		if err != nil || pace < 0.5 || pace > 2 {
			return nil, errors.New("pace must be between 0.5 and 2")
		}
		params["pace"] = pace
	}

	if raw := strings.TrimSpace(r.PostFormValue("pitch")); raw != "" {
		pitch, err := strconv.Atoi(raw)
		if err != nil || pitch < -12 || pitch > 12 {
			return nil, errors.New("pitch must be between -12 and 12 semitones")
		}
		params["pitch"] = pitch
	}

	if raw := strings.TrimSpace(r.PostFormValue("expressiveness")); raw != "" {
		expr, err := strconv.ParseFloat(raw, 64)
		if err != nil || expr < 0 || expr > 1 {
			return nil, errors.New("expressiveness must be between 0 and 1")
		}
		params["expressiveness"] = expr
	}

	if r.PostFormValue("normalize") != "" {
		params["normalize"] = true
	}
	if r.PostFormValue("output_48k") != "" {
		params["output_48k"] = true
	}

	if len(params) == 0 {
		return nil, nil
	}
	return params, nil
}

// handleDownloadAudio answers GET /jobs/{id}/audio by streaming the saved WAV file.
func (s *Server) handleDownloadAudio(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}

	job, err := s.jobs.Get(r.Context(), id, userID)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		serverError(w, r, err)
		return
	}
	if job.UserID != userID {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	if job.Status != jobs.StatusReady || job.AudioPath == "" {
		http.Error(w, "audio not ready", http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(job.AudioPath); err != nil {
		http.Error(w, "audio file missing", http.StatusNotFound)
		return
	}

	ext := job.Format
	if ext == "" {
		ext = "wav"
	}
	contentType := "audio/" + ext
	if ext == "wav" {
		contentType = "audio/wav"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"job-%d.%s\"", job.ID, ext))
	http.ServeFile(w, r, job.AudioPath)
}

// handleDeleteJob answers DELETE /jobs/{id} by removing the job from the DB
// and removing its audio file if present.
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}

	deleted, err := s.jobs.Delete(r.Context(), id, userID)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		serverError(w, r, err)
		return
	}

	paths := deleted.InputPaths()
	if deleted.AudioPath != "" {
		paths = append(paths, deleted.AudioPath)
	}
	removePaths(paths)

	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}

	items, err := s.jobs.ListForUser(r.Context(), userID, queueLimit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	s.renderQueue(w, r, items, 0)
}
