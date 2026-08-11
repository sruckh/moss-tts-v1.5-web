// Package worker owns the background submission loop.
//
// This is the only place in Timbre that calls RunPod. A browser request enqueues
// a row and returns immediately; this goroutine drains those rows out-of-band,
// so the minutes-long render never sits inside an HTTP request Cloudflare would
// cut off at 90s.
//
// Goal 4 ends at submission: the loop stores the RunPod id and advances the
// status to submitted/in_progress. Collecting the finished audio is the
// poller's job.
package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/runpod"
	"github.com/sruckh/timbre/internal/voices"
)

// DefaultInterval is how often the loop looks for queued work. Submission is
// cheap (it only enqueues at RunPod), so this is about latency to first render,
// not load.
const DefaultInterval = 2 * time.Second

// DefaultMaxAttempts bounds retries of a transient submission failure before
// the job is failed outright. Without it a permanently unreachable endpoint
// would keep a job queued forever, which reads to the user as a hang.
const DefaultMaxAttempts = 3

// JobStore is the slice of the jobs store the loop needs. Narrowing it here
// keeps the worker's tests free of a database.
type JobStore interface {
	InFlight(ctx context.Context) (int, error)
	ClaimQueued(ctx context.Context, limit int) ([]jobs.Job, error)
	MarkSubmitted(ctx context.Context, id int64, runpodID, status string) (bool, error)
	MarkFailed(ctx context.Context, id int64, reason string) error
	NoteAttempt(ctx context.Context, id int64, reason string) error
}

// ReferenceStore supplies a cloned voice's stored reference audio.
// ReferenceStore supplies a cloned voice's stored reference audio and its
// Higgs transcript. *voices.Store satisfies this without modification — Get
// and SetReferenceTranscript already exist there.
type ReferenceStore interface {
	Reference(ctx context.Context, id int64) (data []byte, format string, err error)
	Get(ctx context.Context, id int64) (voices.Voice, error)
	SetReferenceTranscript(ctx context.Context, id int64, transcript string) error
}

// Submitter is the RunPod submission call.
type Submitter interface {
	Submit(ctx context.Context, in runpod.Input) (runpod.Submission, error)
	SubmitHiggs(ctx context.Context, in runpod.HiggsInput) (runpod.Submission, error)
}

// DefaultWhisperURL is the private whisper-server sidecar's base URL. It is
// only reachable on shared_net (no host port is published), so this is a
// fixed internal DNS name, not something an operator configures per ADR 002.
const DefaultWhisperURL = "http://whisper-server:8080"

// WhisperTimeout bounds a single POST /inference call. whisper-server has no
// queue of its own worth waiting on indefinitely; a stuck request must not
// stall the submission loop.
const WhisperTimeout = 30 * time.Second

// WhisperClaimExpiry is how long a transcription claim is honored before it
// is considered stale and eligible for recovery (e.g. the process restarted
// mid-attempt and the lease was never released).
const WhisperClaimExpiry = 60 * time.Second

// WhisperMaxAttempts bounds retries of a failing transcription. After this
// many attempts the voice is left without a transcript until a user supplies
// one manually — Higgs jobs referencing it keep failing with a clear reason
// rather than hammering a broken sidecar forever.
const WhisperMaxAttempts = 3

// whisperBackoff returns how long to wait after the given attempt number
// before the voice may be claimed again (attempt 1 is immediate).
func whisperBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 5 * time.Second
	case 2:
		return 15 * time.Second
	default:
		return 0
	}
}

// WhisperTranscriber calls the private whisper-server sidecar to transcribe
// reference audio.
type WhisperTranscriber interface {
	Transcribe(ctx context.Context, data []byte, format string) (string, error)
}

// WhisperAligner performs forced word alignment on completed audio (e.g. PCM WAV bytes)
// via whisper-server, returning structured word timings.
type WhisperAligner interface {
	AlignOutput(ctx context.Context, pcmWav []byte) (*runpod.WordTimings, error)
}

// transcriptionLease tracks one voice's in-process claim state. Timbre runs a
// single worker goroutine (see Tick), so this in-memory map is the atomic
// claim mechanism — there is no second replica it needs to coordinate with,
// and Stage 03 deliberately rejected persisting this transient state in the
// voices table.
type transcriptionLease struct {
	claimedAt time.Time
	attempts  int
}

// httpWhisperClient is the default WhisperTranscriber: a multipart POST to
// whisper-server's /inference endpoint.
type httpWhisperClient struct {
	baseURL string
	timeout time.Duration
	http    *http.Client
}

func NewHTTPWhisperClient(baseURL string, timeout time.Duration) *httpWhisperClient {
	return &httpWhisperClient{baseURL: baseURL, timeout: timeout, http: &http.Client{}}
}

func newHTTPWhisperClient(baseURL string, timeout time.Duration) *httpWhisperClient {
	return NewHTTPWhisperClient(baseURL, timeout)
}

// Transcribe posts audio bytes to whisper-server and returns the trimmed
// transcript text. temperature=0.0 avoids sampling variance across retries;
// response_format=json is what makes the body a single {"text": ...} object.
func (c *httpWhisperClient) Transcribe(ctx context.Context, data []byte, format string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "reference."+format)
	if err != nil {
		return "", fmt.Errorf("whisper: build form file: %w", err)
	}
	if _, err := fw.Write(data); err != nil {
		return "", fmt.Errorf("whisper: write audio: %w", err)
	}
	if err := mw.WriteField("response_format", "json"); err != nil {
		return "", fmt.Errorf("whisper: write response_format field: %w", err)
	}
	if err := mw.WriteField("temperature", "0.0"); err != nil {
		return "", fmt.Errorf("whisper: write temperature field: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("whisper: close multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/inference", &body)
	if err != nil {
		return "", fmt.Errorf("whisper: build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("whisper: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("whisper: decode response: %w", err)
	}
	return strings.TrimSpace(parsed.Text), nil
}

func isPunctuation(s string) bool {
	return strings.Trim(s, ".,!?:;\"'()-_~`«»“”‘’") == ""
}

// AlignOutput posts PCM WAV audio bytes to whisper-server with verbose_json formatting
// and token_timestamps enabled, returning parsed and validated word timings.
func (c *httpWhisperClient) AlignOutput(ctx context.Context, pcmWav []byte) (*runpod.WordTimings, error) {
	if len(pcmWav) == 0 {
		return nil, fmt.Errorf("whisper: empty audio data")
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "output.wav")
	if err != nil {
		return nil, fmt.Errorf("whisper: build form file: %w", err)
	}
	if _, err := fw.Write(pcmWav); err != nil {
		return nil, fmt.Errorf("whisper: write audio: %w", err)
	}
	if err := mw.WriteField("response_format", "verbose_json"); err != nil {
		return nil, fmt.Errorf("whisper: write response_format field: %w", err)
	}
	if err := mw.WriteField("token_timestamps", "true"); err != nil {
		return nil, fmt.Errorf("whisper: write token_timestamps field: %w", err)
	}
	if err := mw.WriteField("temperature", "0.0"); err != nil {
		return nil, fmt.Errorf("whisper: write temperature field: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("whisper: close multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/inference", &body)
	if err != nil {
		return nil, fmt.Errorf("whisper: build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("whisper: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("whisper: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("whisper: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Segments []struct {
			Words []struct {
				Word  string  `json:"word"`
				W     string  `json:"w"`
				Start float64 `json:"start"`
				End   float64 `json:"end"`
			} `json:"words"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("whisper: decode response: %w", err)
	}

	var words []runpod.WordTiming
	var lastEnd float64 = 0

	for _, seg := range parsed.Segments {
		for _, wObj := range seg.Words {
			text := strings.TrimSpace(wObj.Word)
			if text == "" {
				text = strings.TrimSpace(wObj.W)
			}
			if text == "" {
				continue
			}
			start := wObj.Start
			end := wObj.End

			if math.IsNaN(start) || math.IsNaN(end) || math.IsInf(start, 0) || math.IsInf(end, 0) {
				return nil, fmt.Errorf("whisper: non-finite timestamp for word %q: start=%f, end=%f", text, start, end)
			}
			if start < 0 || end < 0 {
				return nil, fmt.Errorf("whisper: negative timestamp for word %q: start=%f, end=%f", text, start, end)
			}

			// If standalone punctuation, attach it to the preceding word if present.
			if isPunctuation(text) {
				if len(words) > 0 {
					words[len(words)-1].W += text
					if end > words[len(words)-1].End {
						words[len(words)-1].End = end
						lastEnd = end
					}
				}
				continue
			}

			if end <= start {
				return nil, fmt.Errorf("whisper: invalid interval for word %q: start=%f, end=%f", text, start, end)
			}
			if len(words) > 0 && start < lastEnd {
				return nil, fmt.Errorf("whisper: overlapping/non-monotonic timestamp for word %q: start=%f < previous end=%f", text, start, lastEnd)
			}

			words = append(words, runpod.WordTiming{
				W:     text,
				Start: start,
				End:   end,
			})
			lastEnd = end
		}
	}

	if len(words) == 0 {
		return nil, nil
	}

	return &runpod.WordTimings{
		Source: "whisper_cpp",
		Words:  words,
	}, nil
}

// Worker drains queued jobs into RunPod.
type Worker struct {
	jobs        JobStore
	voices      ReferenceStore
	client      Submitter
	whisper     WhisperTranscriber
	log         *slog.Logger
	maxInFlight int
	interval    time.Duration
	maxAttempts int

	leaseMu sync.Mutex
	leases  map[int64]*transcriptionLease
}

// Option customizes a Worker.
type Option func(*Worker)

// WithInterval sets the tick period.
func WithInterval(d time.Duration) Option {
	return func(w *Worker) {
		if d > 0 {
			w.interval = d
		}
	}
}

// WithMaxAttempts sets how many times a transient failure is retried.
func WithMaxAttempts(n int) Option {
	return func(w *Worker) {
		if n > 0 {
			w.maxAttempts = n
		}
	}
}

// WithWhisperURL points the default HTTP whisper client at a different base
// URL (e.g. an httptest server in tests). It has no effect if WithWhisperClient
// already replaced the transcriber with a non-HTTP fake.
func WithWhisperURL(url string) Option {
	return func(w *Worker) {
		if url == "" {
			return
		}
		if hc, ok := w.whisper.(*httpWhisperClient); ok {
			hc.baseURL = url
			return
		}
		w.whisper = newHTTPWhisperClient(url, WhisperTimeout)
	}
}

// WithWhisperTimeout overrides the default HTTP whisper client's per-request
// timeout, mainly so tests can exercise timeout handling without waiting 30s.
func WithWhisperTimeout(d time.Duration) Option {
	return func(w *Worker) {
		if d <= 0 {
			return
		}
		if hc, ok := w.whisper.(*httpWhisperClient); ok {
			hc.timeout = d
		}
	}
}

// WithWhisperClient replaces the transcriber outright, e.g. with a fake that
// never touches the network.
func WithWhisperClient(c WhisperTranscriber) Option {
	return func(w *Worker) {
		if c != nil {
			w.whisper = c
		}
	}
}

// New builds a Worker. maxInFlight caps how many jobs may sit at RunPod at once.
func New(jobStore JobStore, referenceStore ReferenceStore, client Submitter,
	maxInFlight int, log *slog.Logger, opts ...Option) *Worker {

	if maxInFlight < 1 {
		maxInFlight = 1
	}
	w := &Worker{
		jobs:        jobStore,
		voices:      referenceStore,
		client:      client,
		whisper:     newHTTPWhisperClient(DefaultWhisperURL, WhisperTimeout),
		log:         log,
		maxInFlight: maxInFlight,
		interval:    DefaultInterval,
		maxAttempts: DefaultMaxAttempts,
		leases:      make(map[int64]*transcriptionLease),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Run drains the queue until ctx is cancelled. It is the goroutine started at
// boot; Tick is exported so tests can drive one pass deterministically.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("submission worker started",
		"interval", w.interval.String(), "max_in_flight", w.maxInFlight)

	// Drain once immediately: jobs queued before a restart should not wait a
	// full interval to move.
	w.Tick(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("submission worker stopped")
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

// Tick submits as many queued jobs as the in-flight budget allows. It runs on a
// single goroutine and submits sequentially, so two ticks can never race to
// submit the same row.
func (w *Worker) Tick(ctx context.Context) {
	inFlight, err := w.jobs.InFlight(ctx)
	if err != nil {
		w.log.Error("worker: count in-flight", "err", err)
		return
	}
	capacity := w.maxInFlight - inFlight
	if capacity <= 0 {
		return
	}

	queued, err := w.jobs.ClaimQueued(ctx, capacity)
	if err != nil {
		w.log.Error("worker: claim queued", "err", err)
		return
	}
	for _, job := range queued {
		if ctx.Err() != nil {
			return
		}
		w.submit(ctx, job)
	}
}

// submit sends one job to RunPod and records the outcome.
// submit sends one job to RunPod and records the outcome.
func (w *Worker) submit(ctx context.Context, job jobs.Job) {
	// Idempotency backstop. ClaimQueued already filters these out; this guards
	// the case where a caller hands us a row directly.
	if job.RunPodID != "" {
		return
	}

	if err := w.ensureTranscript(ctx, job); err != nil {
		w.log.Error("worker: reference transcript unavailable", "job", job.ID, "voice", job.VoiceID, "err", err)
		w.fail(ctx, job.ID, err.Error())
		return
	}

	var submission runpod.Submission
	if job.IsHiggs() {
		higgsInput, err := w.buildHiggsInput(ctx, job)
		if err != nil {
			w.log.Error("worker: build higgs input", "job", job.ID, "err", err)
			w.fail(ctx, job.ID, err.Error())
			return
		}
		submission, err = w.client.SubmitHiggs(ctx, higgsInput)
		if err != nil {
			w.handleSubmitError(ctx, job, err)
			return
		}
	} else {
		input, err := w.buildInput(ctx, job)
		if err != nil {
			// A missing or unreadable reference will not fix itself.
			w.log.Error("worker: build input", "job", job.ID, "err", err)
			w.fail(ctx, job.ID, err.Error())
			return
		}
		submission, err = w.client.Submit(ctx, input)
		if err != nil {
			w.handleSubmitError(ctx, job, err)
			return
		}
	}

	status := jobs.StatusForRunPod(submission.Status)
	claimed, err := w.jobs.MarkSubmitted(ctx, job.ID, submission.ID, status)
	if err != nil {
		// The job is running at RunPod but its id could not be stored, so
		// nothing can ever collect the result. Leaving it queued would
		// re-submit it every tick and bill for each render, so record the
		// failure instead — a stuck job is cheaper than an invisible loop.
		w.log.Error("worker: could not record submission; failing the job",
			"job", job.ID, "runpod_id", submission.ID, "err", err)
		w.fail(ctx, job.ID, "submitted to RunPod but the job id could not be stored: "+err.Error())
		return
	}
	if !claimed {
		// Another path already recorded an id for this row. Nothing to undo —
		// the compare-and-set is what stops a second id from overwriting.
		w.log.Warn("worker: job was already submitted; ignoring duplicate",
			"job", job.ID, "runpod_id", submission.ID)
		return
	}
	w.log.Info("job submitted",
		"job", job.ID, "runpod_id", submission.ID, "status", status, "engine", job.Model)
}

// handleSubmitError decides between failing the job and retrying next tick.
func (w *Worker) handleSubmitError(ctx context.Context, job jobs.Job, err error) {
	reason := err.Error()
	attempt := job.Attempts + 1

	// A missing key or a rejected credential is permanent: fail immediately
	// rather than leaving the job queued forever, which reads as a hang.
	if runpod.IsPermanent(err) {
		w.log.Error("worker: submission rejected", "job", job.ID, "err", err)
		w.fail(ctx, job.ID, reason)
		return
	}
	if attempt >= w.maxAttempts {
		w.log.Error("worker: submission failed permanently",
			"job", job.ID, "attempts", attempt, "err", err)
		w.fail(ctx, job.ID, reason)
		return
	}

	w.log.Warn("worker: submission failed; will retry",
		"job", job.ID, "attempt", attempt, "err", err)
	if err := w.jobs.NoteAttempt(ctx, job.ID, reason); err != nil {
		w.log.Error("worker: note attempt", "job", job.ID, "err", err)
	}
}

func (w *Worker) fail(ctx context.Context, id int64, reason string) {
	// Use a background context: when the app is shutting down, the job still
	// needs its verdict written rather than being left mid-flight.
	writeCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		writeCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	if err := w.jobs.MarkFailed(writeCtx, id, reason); err != nil {
		w.log.Error("worker: mark failed", "job", id, "err", err)
	}
}

// buildInput assembles the RunPod input object for one job, base64-encoding the
// cloned voice's stored reference bytes inline. Stock voices carry no
// reference; RunPod then renders in the model's own voice.
func (w *Worker) buildInput(ctx context.Context, job jobs.Job) (runpod.Input, error) {
	input := runpod.Input{
		Text:     job.Text,
		Language: job.Language,
		Stream:   false,
		Extra:    job.Params(),
	}

	if job.VoiceID == 0 {
		return input, nil
	}

	data, format, err := w.voices.Reference(ctx, job.VoiceID)
	switch {
	case errors.Is(err, voices.ErrNoReference):
		// A stock voice. Nothing to attach.
		return input, nil
	case errors.Is(err, voices.ErrNotFound):
		// The voice was deleted between enqueue and submit. Render without it
		// rather than failing a job the user already paid attention to.
		w.log.Warn("worker: voice missing at submit time; rendering without reference",
			"job", job.ID, "voice", job.VoiceID)
		return input, nil
	case err != nil:
		return runpod.Input{}, err
	}

	input.ReferenceAudioBase64 = base64.StdEncoding.EncodeToString(data)
	input.ReferenceFormat = format
	return input, nil
}

// buildHiggsInput assembles the Higgs engine payload. ensureTranscript has
// already guaranteed a non-blank transcript for any cloned voice by the time
// this runs, so a missing transcript here is a hard error, not a fallback.
func (w *Worker) buildHiggsInput(ctx context.Context, job jobs.Job) (runpod.HiggsInput, error) {
	input := runpod.HiggsInput{
		Text:           job.Text,
		Voice:          "default", // the engine forbids null/empty voice
		ResponseFormat: "wav",
		Speed:          1.0,
		Temperature:    0.8,
		TopK:           50,
	}

	if job.VoiceID == 0 {
		return input, nil // stock Higgs voice, no reference clip
	}

	data, format, err := w.voices.Reference(ctx, job.VoiceID)
	switch {
	case errors.Is(err, voices.ErrNoReference):
		// A stock voice selected under a Higgs job. Nothing to attach.
		return input, nil
	case errors.Is(err, voices.ErrNotFound):
		w.log.Warn("worker: voice missing at submit time; rendering without reference",
			"job", job.ID, "voice", job.VoiceID)
		return input, nil
	case err != nil:
		return runpod.HiggsInput{}, err
	}

	voice, err := w.voices.Get(ctx, job.VoiceID)
	if err != nil {
		return runpod.HiggsInput{}, fmt.Errorf("load voice %d for transcript: %w", job.VoiceID, err)
	}
	if !voice.ReferenceTranscript.Valid || strings.TrimSpace(voice.ReferenceTranscript.V) == "" {
		return runpod.HiggsInput{}, fmt.Errorf("higgs voice %d has no reference transcript", job.VoiceID)
	}

	input.References = []runpod.HiggsReference{{
		Audio:  data,
		Text:   strings.TrimSpace(voice.ReferenceTranscript.V),
		Format: format,
	}}
	return input, nil
}

// ensureTranscript guarantees a non-empty reference_transcript exists before a
// job that needs one reaches RunPod. MOSS-TTS v1.5 jobs never read reference
// transcripts, so they bypass this entirely — a total whisper-server outage
// must never affect MOSS availability.
func (w *Worker) ensureTranscript(ctx context.Context, job jobs.Job) error {
	if job.Model == "" || job.Model == jobs.DefaultModel {
		return nil // MOSS bypass
	}
	if job.VoiceID == 0 {
		return nil // no reference audio in play
	}

	voice, err := w.voices.Get(ctx, job.VoiceID)
	if err != nil {
		if errors.Is(err, voices.ErrNotFound) {
			// The voice was deleted between enqueue and submit. buildInput
			// already renders without it rather than failing the job.
			return nil
		}
		return fmt.Errorf("load voice %d: %w", job.VoiceID, err)
	}
	if voice.Kind != voices.KindCloned {
		return nil // stock voices carry no reference audio to transcribe
	}
	if voice.ReferenceTranscript.Valid && strings.TrimSpace(voice.ReferenceTranscript.V) != "" {
		return nil // already ready, including manual corrections
	}

	w.log.Info("lazy transcription triggered", "voice_id", job.VoiceID, "job_id", job.ID)
	return w.transcribeVoice(ctx, job.VoiceID)
}

// transcribeVoice performs one leased transcription attempt against
// whisper-server for a cloned voice's stored reference audio.
func (w *Worker) transcribeVoice(ctx context.Context, voiceID int64) error {
	if !w.claimTranscription(voiceID) {
		return fmt.Errorf("reference audio transcription for voice %d is not available yet (retry pending)", voiceID)
	}

	data, format, err := w.voices.Reference(ctx, voiceID)
	if err != nil {
		return fmt.Errorf("reference audio transcription failed: %w", err)
	}

	text, err := w.whisper.Transcribe(ctx, data, format)
	if err != nil {
		return fmt.Errorf("reference audio transcription failed: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("no speech detected in reference audio clip")
	}

	if err := w.voices.SetReferenceTranscript(ctx, voiceID, text); err != nil {
		return fmt.Errorf("reference audio transcription failed: %w", err)
	}

	w.log.Info("transcription complete", "voice_id", voiceID, "chars", len(text))
	w.clearTranscriptionClaim(voiceID)
	return nil
}

// claimTranscription atomically leases voiceID for one transcription attempt.
// It reports false when a claim is still active, still inside its backoff
// window, or has exhausted WhisperMaxAttempts. A single worker goroutine
// submits jobs sequentially (see Tick), so this in-memory map cannot race —
// it is the atomic claim Stage 04 describes, just held in the process rather
// than in SQLite (Stage 03 rejected persisting this transient state).
func (w *Worker) claimTranscription(voiceID int64) bool {
	w.leaseMu.Lock()
	defer w.leaseMu.Unlock()

	now := time.Now()
	lease, ok := w.leases[voiceID]
	if ok {
		if now.Sub(lease.claimedAt) >= WhisperClaimExpiry {
			// Stale-claim recovery: the previous attempt never completed
			// (e.g. a crash mid-inference). Start the attempt count over.
			lease.attempts = 0
		} else {
			if lease.attempts >= WhisperMaxAttempts {
				return false
			}
			if now.Before(lease.claimedAt.Add(whisperBackoff(lease.attempts))) {
				return false
			}
		}
	} else {
		lease = &transcriptionLease{}
		w.leases[voiceID] = lease
	}
	lease.claimedAt = now
	lease.attempts++
	return true
}

// clearTranscriptionClaim drops a voice's lease once it has a durable
// transcript and never needs to be claimed again.
func (w *Worker) clearTranscriptionClaim(voiceID int64) {
	w.leaseMu.Lock()
	defer w.leaseMu.Unlock()
	delete(w.leases, voiceID)
}
