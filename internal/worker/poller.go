package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/runpod"
)

// DefaultPollerInterval is how often the poller checks RunPod for in-flight jobs.
const DefaultPollerInterval = 2 * time.Second

// PollerJobStore is the database interface needed by the status poller.
type PollerJobStore interface {
	ListPendingRunPod(ctx context.Context, limit int) ([]jobs.Job, error)
	UpdateStatus(ctx context.Context, id int64, status string) error
	MarkReady(ctx context.Context, id int64, audioPath, format string, sampleRate int, delayMS, execMS int64, alignmentJSON string) error
	MarkPollerFailed(ctx context.Context, id int64, reason string) error
}

// StatusClient is the RunPod status query interface.
type StatusClient interface {
	Status(ctx context.Context, id string) (runpod.StatusResult, error)
	StatusHiggs(ctx context.Context, id string) (runpod.StatusResult, error)
	StatusBreeze(ctx context.Context, id string) (runpod.StatusResult, error)
	StatusAuK(ctx context.Context, id string) (runpod.StatusResult, error)
}

// Poller checks RunPod status for submitted/in_progress jobs, saves audio on completion,
// and updates job state in the database.
type Poller struct {
	jobs     PollerJobStore
	client   StatusClient
	aligner  WhisperAligner
	audioDir string
	log      *slog.Logger
	interval time.Duration
}

// PollerOption customizes a Poller.
type PollerOption func(*Poller)

// WithPollerInterval sets the poller tick duration.
func WithPollerInterval(d time.Duration) PollerOption {
	return func(p *Poller) {
		if d > 0 {
			p.interval = d
		}
	}
}

// WithPollerAligner sets the OutputAligner used for completed Higgs audio renders.
func WithPollerAligner(a WhisperAligner) PollerOption {
	return func(p *Poller) {
		p.aligner = a
	}
}

// NewPoller creates a new status Poller.
func NewPoller(jobStore PollerJobStore, client StatusClient, audioDir string, log *slog.Logger, opts ...PollerOption) *Poller {
	if audioDir == "" {
		audioDir = "/data/audio"
	}
	p := &Poller{
		jobs:     jobStore,
		client:   client,
		audioDir: audioDir,
		log:      log,
		interval: DefaultPollerInterval,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Run executes the polling loop until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	p.log.Info("status poller started", "interval", p.interval.String())

	p.Tick(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.log.Info("status poller stopped")
			return
		case <-ticker.C:
			p.Tick(ctx)
		}
	}
}

// Tick performs one polling pass over pending RunPod jobs.
func (p *Poller) Tick(ctx context.Context) {
	pending, err := p.jobs.ListPendingRunPod(ctx, 50)
	if err != nil {
		p.log.Error("poller: list pending runpod jobs", "err", err)
		return
	}

	for _, job := range pending {
		if ctx.Err() != nil {
			return
		}
		p.pollOne(ctx, job)
	}
}

func (p *Poller) pollOne(ctx context.Context, job jobs.Job) {
	var res runpod.StatusResult
	var err error
	switch {
	case job.IsHiggs():
		res, err = p.client.StatusHiggs(ctx, job.RunPodID)
	case job.IsBreeze():
		res, err = p.client.StatusBreeze(ctx, job.RunPodID)
	case job.IsAuK():
		res, err = p.client.StatusAuK(ctx, job.RunPodID)
	default:
		res, err = p.client.Status(ctx, job.RunPodID)
	}
	if err != nil {
		if runpod.IsPermanent(err) {
			p.log.Error("poller: status query rejected permanently", "job", job.ID, "runpod_id", job.RunPodID, "err", err)
			p.fail(ctx, job.ID, err.Error())
		} else {
			p.log.Warn("poller: status query transient error", "job", job.ID, "runpod_id", job.RunPodID, "err", err)
		}
		return
	}

	switch res.Status {
	case runpod.StatusInQueue:
		return

	case runpod.StatusInProgress:
		if job.Status != jobs.StatusInProgress {
			if err := p.jobs.UpdateStatus(ctx, job.ID, jobs.StatusInProgress); err != nil {
				p.log.Error("poller: update status to in_progress", "job", job.ID, "err", err)
			}
		}

	case runpod.StatusFailed, runpod.StatusCancelled, runpod.StatusTimedOut:
		reason := res.ErrorString()
		// Breeze's structured failure envelope can land nested in
		// Output.Error rather than at the top level (RunPod's runtime only
		// lifts a top-level "error" key); ErrorString never looks there, so a
		// nested envelope would otherwise be silently discarded in favor of
		// the generic fallback below.
		if job.IsBreeze() {
			if env := res.BreezeError(); env != nil {
				reason = env.Error()
			}
		}
		if job.IsAuK() {
			if env := res.AuKError(); env != nil {
				reason = env.Error()
			}
		}
		if reason == "" {
			reason = "RunPod execution failed"
		}
		p.log.Error("poller: job failed at RunPod", "job", job.ID, "runpod_id", job.RunPodID, "reason", reason)
		p.fail(ctx, job.ID, reason)

	case runpod.StatusCompleted:
		audioData, err := outputAudio(ctx, res.Output)
		if err != nil {
			p.log.Error("poller: collect completed audio", "job", job.ID, "err", err)
			p.fail(ctx, job.ID, err.Error())
			return
		}

		dir := filepath.Join(p.audioDir, "renders")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			p.log.Error("poller: create renders dir", "dir", dir, "err", err)
			p.fail(ctx, job.ID, "failed to create audio output directory: "+err.Error())
			return
		}

		ext := res.Output.Format
		if ext == "" {
			ext = "wav"
		}
		filename := fmt.Sprintf("job_%d.%s", job.ID, ext)
		fullPath := filepath.Join(dir, filename)

		if err := os.WriteFile(fullPath, audioData, 0o640); err != nil {
			p.log.Error("poller: write audio file", "path", fullPath, "err", err)
			p.fail(ctx, job.ID, "failed to save audio file: "+err.Error())
			return
		}

		sampleRate := res.Output.SampleRate
		if sampleRate <= 0 {
			sampleRate = 24000
		}

		// word_timings is optional: the worker omits it for streaming renders,
		// older builds, or failed alignment. nil ⇒ empty string ⇒ the player
		// interpolates word positions. A marshal failure is treated like absence
		// — it never fails a job that already has good audio.
		//
		// For completed Higgs and Breeze jobs, word alignment is performed via
		// the local Whisper aligner on the saved PCM WAV bytes. Breeze carries
		// no native timings at all — its worker's success payload has no
		// word_timings field — so the aligner is the only source it has. MOSS
		// completion payloads bypass local alignment and preserve native
		// word_timings verbatim.
		alignmentJSON := ""
		if job.IsHiggs() || job.IsBreeze() || shouldAlignAuK(job, res.Output) {
			if p.aligner != nil {
				wt, err := p.aligner.AlignOutput(ctx, audioData)
				if err != nil {
					p.log.Warn("poller: output word alignment failed", "job", job.ID, "model", job.Model, "err", err)
				} else if wt != nil {
					if b, err := json.Marshal(wt); err == nil {
						alignmentJSON = string(b)
					}
				}
			}
		} else {
			if res.Output.WordTimings != nil {
				if b, err := json.Marshal(res.Output.WordTimings); err == nil {
					alignmentJSON = string(b)
				}
			}
		}

		if err := p.jobs.MarkReady(ctx, job.ID, fullPath, ext, sampleRate, res.DelayTime, res.ExecutionTime, alignmentJSON); err != nil {
			p.log.Error("poller: mark ready", "job", job.ID, "err", err)
			return
		}
		p.log.Info("job ready", "job", job.ID, "runpod_id", job.RunPodID, "path", fullPath)
	}
}


const maxRunPodOutputBytes = 64 << 20

func outputAudio(ctx context.Context, output runpod.Output) ([]byte, error) {
	if output.AudioBase64 != "" {
		data, err := base64.StdEncoding.DecodeString(output.AudioBase64)
		if err != nil {
			return nil, fmt.Errorf("failed to decode audio base64: %w", err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("RunPod output contains empty audio data")
		}
		if len(data) > maxRunPodOutputBytes {
			return nil, fmt.Errorf("RunPod output exceeds the %d byte limit", maxRunPodOutputBytes)
		}
		return data, nil
	}
	if output.AudioURL == "" {
		return nil, fmt.Errorf("RunPod output contains no audio data")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, output.AudioURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build audio download request: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download RunPod audio: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("download RunPod audio: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxRunPodOutputBytes {
		return nil, fmt.Errorf("RunPod output exceeds the %d byte limit", maxRunPodOutputBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRunPodOutputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read RunPod audio: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("RunPod output contains empty audio data")
	}
	if len(data) > maxRunPodOutputBytes {
		return nil, fmt.Errorf("RunPod output exceeds the %d byte limit", maxRunPodOutputBytes)
	}
	return data, nil
}


func shouldAlignAuK(job jobs.Job, output runpod.Output) bool {
	if !job.IsAuK() {
		return false
	}
	task := output.TaskExecuted
	if task == "" {
		params := job.Params()
		task, _ = params["task"].(string)
		if task == "" || task == runpod.AuKTaskAuto {
			if _, ok := params["prompt_audio"]; ok {
				task = runpod.AuKTaskZeroShotTTS
			} else if _, ok := params["prompt_audio_path"]; ok {
				task = runpod.AuKTaskZeroShotTTS
			} else {
				task = runpod.AuKTaskInstructTTS
			}
		}
	}
	return task == runpod.AuKTaskZeroShotTTS || task == runpod.AuKTaskInstructTTS
}

func (p *Poller) fail(ctx context.Context, id int64, reason string) {
	writeCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		writeCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	if err := p.jobs.MarkPollerFailed(writeCtx, id, reason); err != nil {
		p.log.Error("poller: mark failed", "job", id, "err", err)
	}
}
