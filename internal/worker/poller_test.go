package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/runpod"
)

type mockPollerStore struct {
	pending    []jobs.Job
	updated    map[int64]string
	ready      map[int64]string
	failed     map[int64]string
	audioPaths map[int64]string
	alignment  map[int64]string
}

func newMockPollerStore(pending ...jobs.Job) *mockPollerStore {
	return &mockPollerStore{
		pending:    pending,
		updated:    make(map[int64]string),
		ready:      make(map[int64]string),
		failed:     make(map[int64]string),
		audioPaths: make(map[int64]string),
		alignment:  make(map[int64]string),
	}
}

func (m *mockPollerStore) ListPendingRunPod(ctx context.Context, limit int) ([]jobs.Job, error) {
	return m.pending, nil
}

func (m *mockPollerStore) UpdateStatus(ctx context.Context, id int64, status string) error {
	m.updated[id] = status
	return nil
}

func (m *mockPollerStore) MarkReady(ctx context.Context, id int64, audioPath, format string, sampleRate int, delayMS, execMS int64, alignmentJSON string) error {
	m.ready[id] = jobs.StatusReady
	m.audioPaths[id] = audioPath
	m.alignment[id] = alignmentJSON
	return nil
}

func (m *mockPollerStore) MarkPollerFailed(ctx context.Context, id int64, reason string) error {
	m.failed[id] = reason
	return nil
}

type mockStatusClient struct {
	fn func(ctx context.Context, id string) (runpod.StatusResult, error)
}

func (m *mockStatusClient) Status(ctx context.Context, id string) (runpod.StatusResult, error) {
	return m.fn(ctx, id)
}

func (m *mockStatusClient) StatusHiggs(ctx context.Context, id string) (runpod.StatusResult, error) {
	return m.fn(ctx, id)
}

func TestPollerCompletesJobAndSavesAudioFile(t *testing.T) {
	tempDir := t.TempDir()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	job := jobs.Job{
		ID:       42,
		UserID:   1,
		Status:   jobs.StatusSubmitted,
		RunPodID: "rp-42",
	}

	store := newMockPollerStore(job)
	wavBytes := []byte("RIFFxxxxWAVEfmt ")
	encodedWav := base64.StdEncoding.EncodeToString(wavBytes)

	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			if id != "rp-42" {
				t.Fatalf("unexpected id: %s", id)
			}
			return runpod.StatusResult{
				ID:            "rp-42",
				Status:        runpod.StatusCompleted,
				DelayTime:     150,
				ExecutionTime: 850,
				Output: runpod.Output{
					AudioBase64: encodedWav,
					Format:      "wav",
					SampleRate:  24000,
				},
			}, nil
		},
	}

	poller := NewPoller(store, client, tempDir, log, WithPollerInterval(10))
	poller.Tick(context.Background())

	if store.ready[42] != jobs.StatusReady {
		t.Errorf("job 42 status = %q, want ready", store.ready[42])
	}

	savedPath := store.audioPaths[42]
	if savedPath == "" {
		t.Fatal("audioPath for job 42 was empty")
	}

	savedData, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("os.ReadFile(%s): %v", savedPath, err)
	}

	if string(savedData) != string(wavBytes) {
		t.Errorf("saved content = %q, want %q", savedData, wavBytes)
	}

	wantFilename := filepath.Join(tempDir, "renders", "job_42.wav")
	if savedPath != wantFilename {
		t.Errorf("savedPath = %q, want %q", savedPath, wantFilename)
	}
}

func TestPollerHandlesFailedStatus(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	job := jobs.Job{
		ID:       99,
		UserID:   1,
		Status:   jobs.StatusInProgress,
		RunPodID: "rp-99",
	}

	store := newMockPollerStore(job)
	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{
				ID:     "rp-99",
				Status: runpod.StatusFailed,
				Error:  "GPU out of memory",
			}, nil
		},
	}

	poller := NewPoller(store, client, t.TempDir(), log)
	poller.Tick(context.Background())

	if reason, ok := store.failed[99]; !ok || reason != "GPU out of memory" {
		t.Errorf("failed[99] = %q, want 'GPU out of memory'", reason)
	}
}

func TestPollerUpdatesInProgressStatus(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	job := jobs.Job{
		ID:       10,
		UserID:   1,
		Status:   jobs.StatusSubmitted,
		RunPodID: "rp-10",
	}

	store := newMockPollerStore(job)
	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{
				ID:     "rp-10",
				Status: runpod.StatusInProgress,
			}, nil
		},
	}

	poller := NewPoller(store, client, t.TempDir(), log)
	poller.Tick(context.Background())

	if status, ok := store.updated[10]; !ok || status != jobs.StatusInProgress {
		t.Errorf("updated[10] = %q, want in_progress", status)
	}
}

func TestPollerPermanentErrorFailsJob(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	job := jobs.Job{
		ID:       11,
		UserID:   1,
		Status:   jobs.StatusSubmitted,
		RunPodID: "rp-11",
	}

	store := newMockPollerStore(job)
	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{}, &runpod.Error{StatusCode: 404, Body: "not found"}
		},
	}

	poller := NewPoller(store, client, t.TempDir(), log)
	poller.Tick(context.Background())

	if reason, ok := store.failed[11]; !ok || reason == "" {
		t.Errorf("failed[11] = %q, want recorded failure", reason)
	}
}

// A completed job carrying word_timings hands the marshaled block to MarkReady,
// so it lands on the row the player reads. A job whose payload omits
// word_timings (streaming render, older worker, failed alignment) hands an
// empty string — the player then interpolates.
func TestPollerThreadsWordTimings(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	encodedWav := base64.StdEncoding.EncodeToString([]byte("RIFFxxxxWAVEfmt "))

	// Present: the block flows through to the store.
	store := newMockPollerStore(jobs.Job{ID: 77, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-77"})
	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{
				ID:     "rp-77",
				Status: runpod.StatusCompleted,
				Output: runpod.Output{
					AudioBase64: encodedWav, Format: "wav", SampleRate: 24000,
					WordTimings: &runpod.WordTimings{
						FrameRate: 50, Source: "mms_fa_forced_alignment",
						Words: []runpod.WordTiming{{W: "Hi.", Start: 0, End: 0.3}},
					},
				},
			}, nil
		},
	}
	NewPoller(store, client, t.TempDir(), log, WithPollerInterval(10)).Tick(context.Background())

	if store.ready[77] != jobs.StatusReady {
		t.Fatal("job 77 was not marked ready")
	}
	if got := store.alignment[77]; !strings.Contains(got, `"words"`) || !strings.Contains(got, `"Hi."`) {
		t.Errorf("alignment[77] = %q, want JSON carrying the word_timings words", got)
	}

	// Absent: empty alignment string (the player interpolates).
	store2 := newMockPollerStore(jobs.Job{ID: 78, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-78"})
	client2 := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{
				ID:     "rp-78",
				Status: runpod.StatusCompleted,
				Output: runpod.Output{AudioBase64: encodedWav, Format: "wav", SampleRate: 24000},
			}, nil
		},
	}
	NewPoller(store2, client2, t.TempDir(), log, WithPollerInterval(10)).Tick(context.Background())

	if store2.ready[78] != jobs.StatusReady {
		t.Fatal("job 78 was not marked ready")
	}
	if got := store2.alignment[78]; got != "" {
		t.Errorf("alignment[78] = %q, want empty when word_timings is absent", got)
	}
}

// routingStatusClient records which status method was called so a test can
// assert the poller routes by engine model.
type routingStatusClient struct {
	statusCalls      int
	statusHiggsCalls int
	result           runpod.StatusResult
	err              error
}

func (r *routingStatusClient) Status(context.Context, string) (runpod.StatusResult, error) {
	r.statusCalls++
	return r.result, r.err
}

func (r *routingStatusClient) StatusHiggs(context.Context, string) (runpod.StatusResult, error) {
	r.statusHiggsCalls++
	return r.result, r.err
}

// TestPollerRoutesByEngineModel asserts a Higgs job is polled through
// StatusHiggs and a MOSS job through Status.
func TestPollerRoutesByEngineModel(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	cases := []struct {
		name      string
		model     string
		wantMoss  int
		wantHiggs int
	}{
		{"moss", jobs.DefaultModel, 1, 0},
		{"blank defaults to moss", "", 1, 0},
		{"higgs", jobs.HiggsModel, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := jobs.Job{ID: 7, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-7", Model: tc.model}
			store := newMockPollerStore(job)
			client := &routingStatusClient{result: runpod.StatusResult{ID: "rp-7", Status: runpod.StatusInQueue}}

			NewPoller(store, client, t.TempDir(), log, WithPollerInterval(10)).Tick(context.Background())

			if client.statusCalls != tc.wantMoss {
				t.Errorf("Status calls = %d, want %d", client.statusCalls, tc.wantMoss)
			}
			if client.statusHiggsCalls != tc.wantHiggs {
				t.Errorf("StatusHiggs calls = %d, want %d", client.statusHiggsCalls, tc.wantHiggs)
			}
		})
	}
}

type mockAligner struct {
	fn func(ctx context.Context, pcmWav []byte) (*runpod.WordTimings, error)
}

func (m *mockAligner) AlignOutput(ctx context.Context, pcmWav []byte) (*runpod.WordTimings, error) {
	return m.fn(ctx, pcmWav)
}

func TestPollerAlignsCompletedHiggsWAV(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	wavBytes := []byte("RIFFxxxxWAVEfmt ")
	encodedWav := base64.StdEncoding.EncodeToString(wavBytes)

	job := jobs.Job{ID: 88, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-88", Model: jobs.HiggsModel}
	store := newMockPollerStore(job)
	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{
				ID:            "rp-88",
				Status:        runpod.StatusCompleted,
				DelayTime:     100,
				ExecutionTime: 500,
				Output: runpod.Output{
					AudioBase64: encodedWav,
					Format:      "wav",
					SampleRate:  24000,
				},
			}, nil
		},
	}

	var alignedBytes []byte
	aligner := &mockAligner{
		fn: func(ctx context.Context, pcmWav []byte) (*runpod.WordTimings, error) {
			alignedBytes = pcmWav
			return &runpod.WordTimings{
				Source: "whisper_cpp",
				Words:  []runpod.WordTiming{{W: "Hello", Start: 0.1, End: 0.5}},
			}, nil
		},
	}

	poller := NewPoller(store, client, t.TempDir(), log, WithPollerAligner(aligner), WithPollerInterval(10))
	poller.Tick(context.Background())

	if store.ready[88] != jobs.StatusReady {
		t.Fatal("job 88 was not marked ready")
	}
	if string(alignedBytes) != string(wavBytes) {
		t.Errorf("alignedBytes = %q, want %q", alignedBytes, wavBytes)
	}
	gotAlignment := store.alignment[88]
	if !strings.Contains(gotAlignment, `"whisper_cpp"`) || !strings.Contains(gotAlignment, `"Hello"`) {
		t.Errorf("alignment[88] = %q, want JSON with whisper_cpp source and word Hello", gotAlignment)
	}
}

func TestPollerHiggsAlignmentFailureStillMarksReady(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	wavBytes := []byte("RIFFxxxxWAVEfmt ")
	encodedWav := base64.StdEncoding.EncodeToString(wavBytes)

	job := jobs.Job{ID: 89, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-89", Model: jobs.HiggsModel}
	store := newMockPollerStore(job)
	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{
				ID:     "rp-89",
				Status: runpod.StatusCompleted,
				Output: runpod.Output{AudioBase64: encodedWav, Format: "wav", SampleRate: 24000},
			}, nil
		},
	}

	aligner := &mockAligner{
		fn: func(ctx context.Context, pcmWav []byte) (*runpod.WordTimings, error) {
			return nil, errors.New("whisper sidecar timeout")
		},
	}

	poller := NewPoller(store, client, t.TempDir(), log, WithPollerAligner(aligner), WithPollerInterval(10))
	poller.Tick(context.Background())

	if store.ready[89] != jobs.StatusReady {
		t.Fatal("job 89 was not marked ready on alignment error")
	}
	if store.alignment[89] != "" {
		t.Errorf("alignment[89] = %q, want empty string when alignment fails", store.alignment[89])
	}
	if store.failed[89] != "" {
		t.Errorf("failed[89] = %q, want job not failed on alignment error", store.failed[89])
	}
}

func TestPollerMOSSBypassesLocalAlignment(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	wavBytes := []byte("RIFFxxxxWAVEfmt ")
	encodedWav := base64.StdEncoding.EncodeToString(wavBytes)

	job := jobs.Job{ID: 90, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-90", Model: jobs.DefaultModel}
	store := newMockPollerStore(job)
	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{
				ID:     "rp-90",
				Status: runpod.StatusCompleted,
				Output: runpod.Output{
					AudioBase64: encodedWav, Format: "wav", SampleRate: 24000,
					WordTimings: &runpod.WordTimings{
						Source: "moss_native",
						Words:  []runpod.WordTiming{{W: "MossWord", Start: 0.0, End: 0.4}},
					},
				},
			}, nil
		},
	}

	alignerCalled := false
	aligner := &mockAligner{
		fn: func(ctx context.Context, pcmWav []byte) (*runpod.WordTimings, error) {
			alignerCalled = true
			return nil, errors.New("should not be called")
		},
	}

	poller := NewPoller(store, client, t.TempDir(), log, WithPollerAligner(aligner), WithPollerInterval(10))
	poller.Tick(context.Background())

	if alignerCalled {
		t.Fatal("local aligner was called for MOSS job, want bypass")
	}
	if store.ready[90] != jobs.StatusReady {
		t.Fatal("job 90 was not marked ready")
	}
	if got := store.alignment[90]; !strings.Contains(got, `"moss_native"`) || !strings.Contains(got, `"MossWord"`) {
		t.Errorf("alignment[90] = %q, want native MOSS timings preserved", got)
	}
}
