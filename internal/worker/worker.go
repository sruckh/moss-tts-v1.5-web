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
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
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
type ReferenceStore interface {
	Reference(ctx context.Context, id int64) (data []byte, format string, err error)
}

// Submitter is the RunPod submission call.
type Submitter interface {
	Submit(ctx context.Context, in runpod.Input) (runpod.Submission, error)
}

// Worker drains queued jobs into RunPod.
type Worker struct {
	jobs        JobStore
	voices      ReferenceStore
	client      Submitter
	log         *slog.Logger
	maxInFlight int
	interval    time.Duration
	maxAttempts int
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
		log:         log,
		maxInFlight: maxInFlight,
		interval:    DefaultInterval,
		maxAttempts: DefaultMaxAttempts,
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
func (w *Worker) submit(ctx context.Context, job jobs.Job) {
	// Idempotency backstop. ClaimQueued already filters these out; this guards
	// the case where a caller hands us a row directly.
	if job.RunPodID != "" {
		return
	}

	input, err := w.buildInput(ctx, job)
	if err != nil {
		// A missing or unreadable reference will not fix itself.
		w.log.Error("worker: build input", "job", job.ID, "err", err)
		w.fail(ctx, job.ID, err.Error())
		return
	}

	submission, err := w.client.Submit(ctx, input)
	if err != nil {
		w.handleSubmitError(ctx, job, err)
		return
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
		"job", job.ID, "runpod_id", submission.ID, "status", status)
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
