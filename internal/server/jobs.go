package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/voices"
	"github.com/sruckh/timbre/internal/web"
)

// queueLimit is how many of a user's jobs the queue fragment shows.
const queueLimit = 10

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
	available, err := s.voices.List(r.Context(), userID)
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}

	voiceID, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("voice_id")), 10, 64)
	if err != nil || voiceID <= 0 {
		http.Error(w, jobs.ErrNoVoice.Error(), http.StatusBadRequest)
		return
	}
	// The voice must exist: a job pinned to a phantom id would only fail later,
	// in the worker, where the user never sees why.
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

	model, err := jobs.ResolveModel(r.PostFormValue("model"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	params, err := parseJobParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := s.jobs.Enqueue(r.Context(), jobs.NewJob{
		UserID:   userID,
		VoiceID:  voiceID,
		Text:     r.PostFormValue("text"),
		Language: r.PostFormValue("language"),
		Model:    model,
		Params:   params,
	})
	if err != nil {
		// Enqueue's validation errors are written for the user.
		if errors.Is(err, jobs.ErrEmptyText) || errors.Is(err, jobs.ErrTextTooLong) ||
			errors.Is(err, jobs.ErrLanguage) || errors.Is(err, jobs.ErrNoVoice) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		serverError(w, r, err)
		return
	}

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
func parseJobParams(r *http.Request) (map[string]any, error) {
	params := map[string]any{}

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

	if deleted.AudioPath != "" {
		_ = os.Remove(deleted.AudioPath)
	}

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
