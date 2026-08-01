package worker

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
}

func newMockPollerStore(pending ...jobs.Job) *mockPollerStore {
	return &mockPollerStore{
		pending:    pending,
		updated:    make(map[int64]string),
		ready:      make(map[int64]string),
		failed:     make(map[int64]string),
		audioPaths: make(map[int64]string),
	}
}

func (m *mockPollerStore) ListPendingRunPod(ctx context.Context, limit int) ([]jobs.Job, error) {
	return m.pending, nil
}

func (m *mockPollerStore) UpdateStatus(ctx context.Context, id int64, status string) error {
	m.updated[id] = status
	return nil
}

func (m *mockPollerStore) MarkReady(ctx context.Context, id int64, audioPath, format string, sampleRate int, delayMS, execMS int64) error {
	m.ready[id] = jobs.StatusReady
	m.audioPaths[id] = audioPath
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
