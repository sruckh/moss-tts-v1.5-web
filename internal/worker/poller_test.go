package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func (m *mockStatusClient) StatusBreeze(ctx context.Context, id string) (runpod.StatusResult, error) {
	return m.fn(ctx, id)
}


func (m *mockStatusClient) StatusAuK(ctx context.Context, id string) (runpod.StatusResult, error) {
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

// TestPollerBreezeFailureUsesNestedErrorEnvelope covers the case ErrorString
// alone cannot: a Breeze worker failure that lands in Output.Error rather than
// the top-level Error RunPod's runtime lifts a handler error into. Without
// consulting BreezeError, this would fall through to the generic
// "RunPod execution failed" and lose the worker's code/field/message.
func TestPollerBreezeFailureUsesNestedErrorEnvelope(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	job := jobs.Job{
		ID:       100,
		UserID:   1,
		Status:   jobs.StatusInProgress,
		RunPodID: "rp-100",
		Model:    jobs.BreezeModel,
	}

	store := newMockPollerStore(job)
	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{
				ID:     "rp-100",
				Status: runpod.StatusFailed,
				Output: runpod.Output{
					Error: json.RawMessage(`{"code":"reference_audio_too_large","field":"reference_audio","message":"clip exceeds 4 MiB decoded"}`),
				},
			}, nil
		},
	}

	poller := NewPoller(store, client, t.TempDir(), log)
	poller.Tick(context.Background())

	reason, ok := store.failed[100]
	if !ok {
		t.Fatal("job 100 was not failed")
	}
	if !strings.Contains(reason, "reference_audio_too_large") || !strings.Contains(reason, "clip exceeds 4 MiB decoded") {
		t.Errorf("failed[100] = %q, want the nested envelope's code and message, not the generic fallback", reason)
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
	statusCalls       int
	statusHiggsCalls  int
	statusBreezeCalls int
	statusAuKCalls    int
	result            runpod.StatusResult
	err               error
}

func (r *routingStatusClient) Status(context.Context, string) (runpod.StatusResult, error) {
	r.statusCalls++
	return r.result, r.err
}

func (r *routingStatusClient) StatusHiggs(context.Context, string) (runpod.StatusResult, error) {
	r.statusHiggsCalls++
	return r.result, r.err
}

func (r *routingStatusClient) StatusBreeze(context.Context, string) (runpod.StatusResult, error) {
	r.statusBreezeCalls++
	return r.result, r.err
}


func (r *routingStatusClient) StatusAuK(context.Context, string) (runpod.StatusResult, error) {
	r.statusAuKCalls++
	return r.result, r.err
}

// TestPollerRoutesByEngineModel asserts a Higgs job is polled through
// StatusHiggs and a MOSS job through Status.
func TestPollerRoutesByEngineModel(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	cases := []struct {
		name                         string
		model                        string
		wantMoss, wantHiggs          int
		wantBreeze, wantAuK          int
	}{
		{"moss", jobs.DefaultModel, 1, 0, 0, 0},
		{"blank defaults to moss", "", 1, 0, 0, 0},
		{"higgs", jobs.HiggsModel, 0, 1, 0, 0},
		{"breeze", jobs.BreezeModel, 0, 0, 1, 0},
		{"auk", jobs.AuKModel, 0, 0, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := jobs.Job{ID: 7, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-7", Model: tc.model}
			store := newMockPollerStore(job)
			client := &routingStatusClient{result: runpod.StatusResult{ID: "rp-7", Status: runpod.StatusInQueue}}

			NewPoller(store, client, t.TempDir(), log, WithPollerInterval(10)).Tick(context.Background())

			if client.statusCalls != tc.wantMoss || client.statusHiggsCalls != tc.wantHiggs ||
				client.statusBreezeCalls != tc.wantBreeze || client.statusAuKCalls != tc.wantAuK {
				t.Errorf("route counts moss/higgs/breeze/auk = %d/%d/%d/%d, want %d/%d/%d/%d",
					client.statusCalls, client.statusHiggsCalls, client.statusBreezeCalls, client.statusAuKCalls,
					tc.wantMoss, tc.wantHiggs, tc.wantBreeze, tc.wantAuK)
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

// TestPollerAlignsCompletedBreezeWAV asserts a completed Breeze job is aligned
// by the local Whisper aligner and the timings land on the row. Breeze carries
// no native timings at all — its worker's success payload has no word_timings
// field — so the aligner is the only source it has. The decoy WordTimings on
// the output proves the payload is never consulted for a Breeze job: even if
// a future worker sent the field, the stored alignment comes from the aligner.
func TestPollerAlignsCompletedBreezeWAV(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	wavBytes := []byte("RIFFxxxxWAVEfmt ")
	encodedWav := base64.StdEncoding.EncodeToString(wavBytes)

	job := jobs.Job{ID: 91, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-91", Model: jobs.BreezeModel}
	store := newMockPollerStore(job)
	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{
				ID:            "rp-91",
				Status:        runpod.StatusCompleted,
				DelayTime:     100,
				ExecutionTime: 500,
				Output: runpod.Output{
					AudioBase64: encodedWav,
					Format:      "wav",
					SampleRate:  24000,
					WordTimings: &runpod.WordTimings{
						Source: "decoy_native",
						Words:  []runpod.WordTiming{{W: "Decoy", Start: 0.0, End: 0.1}},
					},
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
				Words:  []runpod.WordTiming{{W: "Breeze", Start: 0.2, End: 0.9}},
			}, nil
		},
	}

	poller := NewPoller(store, client, t.TempDir(), log, WithPollerAligner(aligner), WithPollerInterval(10))
	poller.Tick(context.Background())

	if store.ready[91] != jobs.StatusReady {
		t.Fatal("job 91 was not marked ready")
	}
	if string(alignedBytes) != string(wavBytes) {
		t.Errorf("alignedBytes = %q, want %q", alignedBytes, wavBytes)
	}
	gotAlignment := store.alignment[91]
	if !strings.Contains(gotAlignment, `"whisper_cpp"`) || !strings.Contains(gotAlignment, `"Breeze"`) {
		t.Errorf("alignment[91] = %q, want JSON with whisper_cpp source and word Breeze", gotAlignment)
	}
	if strings.Contains(gotAlignment, "decoy") || strings.Contains(gotAlignment, "Decoy") {
		t.Errorf("alignment[91] = %q, want the payload's word_timings ignored for Breeze", gotAlignment)
	}
}

// TestPollerBreezeAlignmentFailureStillMarksReady is the Breeze half of the
// fail-open contract: when the whisper sidecar is down the job still reaches
// READY with an empty alignment_json (the player interpolates), and is never
// failed for an alignment problem.
func TestPollerBreezeAlignmentFailureStillMarksReady(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	wavBytes := []byte("RIFFxxxxWAVEfmt ")
	encodedWav := base64.StdEncoding.EncodeToString(wavBytes)

	job := jobs.Job{ID: 92, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-92", Model: jobs.BreezeModel}
	store := newMockPollerStore(job)
	client := &mockStatusClient{
		fn: func(ctx context.Context, id string) (runpod.StatusResult, error) {
			return runpod.StatusResult{
				ID:     "rp-92",
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

	if store.ready[92] != jobs.StatusReady {
		t.Fatal("job 92 was not marked ready on alignment error")
	}
	if store.alignment[92] != "" {
		t.Errorf("alignment[92] = %q, want empty string when alignment fails", store.alignment[92])
	}
	if store.failed[92] != "" {
		t.Errorf("failed[92] = %q, want job not failed on alignment error", store.failed[92])
	}
}


func TestPollerAuKS3CompletionDownloadsAndAlignsTTS(t *testing.T) {
	audio := []byte("RIFF-AUK-DOWNLOAD")
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(audio)
	}))
	defer download.Close()
	job := jobs.Job{ID: 93, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-93", Model: jobs.AuKModel, ParamsJSON: `{"task":"instruct_tts"}`}
	store := newMockPollerStore(job)
	client := &mockStatusClient{fn: func(context.Context, string) (runpod.StatusResult, error) {
		return runpod.StatusResult{Status: runpod.StatusCompleted, Output: runpod.Output{
			Delivery: runpod.AuKDeliveryS3, AudioURL: download.URL + "/audio.wav",
			SampleRate: 24000, TaskExecuted: runpod.AuKTaskInstructTTS,
		}}, nil
	}}
	alignCalls := 0
	aligner := &mockAligner{fn: func(context.Context, []byte) (*runpod.WordTimings, error) {
		alignCalls++
		return &runpod.WordTimings{Words: []runpod.WordTiming{{W: "hello", Start: 0, End: 1}}}, nil
	}}
	NewPoller(store, client, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), WithPollerAligner(aligner)).Tick(context.Background())
	if alignCalls != 1 || store.ready[job.ID] == "" || store.alignment[job.ID] == "" {
		t.Fatalf("alignCalls=%d ready=%q alignment=%q failed=%q", alignCalls, store.ready[job.ID], store.alignment[job.ID], store.failed[job.ID])
	}
	got, err := os.ReadFile(store.audioPaths[job.ID])
	if err != nil || string(got) != string(audio) {
		t.Fatalf("saved audio=%q err=%v", got, err)
	}
}

func TestPollerAuKEditingCompletionSkipsAlignment(t *testing.T) {
	audio := base64.StdEncoding.EncodeToString([]byte("RIFF-EDIT"))
	job := jobs.Job{ID: 94, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-94", Model: jobs.AuKModel, ParamsJSON: `{"task":"content_edit"}`}
	store := newMockPollerStore(job)
	client := &mockStatusClient{fn: func(context.Context, string) (runpod.StatusResult, error) {
		return runpod.StatusResult{Status: runpod.StatusCompleted, Output: runpod.Output{AudioBase64: audio, TaskExecuted: runpod.AuKTaskContentEdit}}, nil
	}}
	alignCalls := 0
	aligner := &mockAligner{fn: func(context.Context, []byte) (*runpod.WordTimings, error) {
		alignCalls++
		return nil, nil
	}}
	NewPoller(store, client, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), WithPollerAligner(aligner)).Tick(context.Background())
	if alignCalls != 0 || store.ready[job.ID] == "" {
		t.Fatalf("alignCalls=%d ready=%q failed=%q", alignCalls, store.ready[job.ID], store.failed[job.ID])
	}
}

func TestPollerAuKFailureUsesJSONStringEnvelope(t *testing.T) {
	job := jobs.Job{ID: 95, UserID: 1, Status: jobs.StatusSubmitted, RunPodID: "rp-95", Model: jobs.AuKModel}
	store := newMockPollerStore(job)
	client := &mockStatusClient{fn: func(context.Context, string) (runpod.StatusResult, error) {
		return runpod.StatusResult{Status: runpod.StatusFailed, Error: `{"code":"audio_too_large","message":"audio exceeds limit","field":"audio"}`}, nil
	}}
	NewPoller(store, client, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil))).Tick(context.Background())
	if got := store.failed[job.ID]; !strings.Contains(got, "audio_too_large") || !strings.Contains(got, "audio exceeds limit") {
		t.Fatalf("failure = %q", got)
	}
}

func TestOutputAudioRejectsOversizeURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "70000000")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if _, err := outputAudio(context.Background(), runpod.Output{AudioURL: server.URL}); err == nil {
		t.Fatal("outputAudio succeeded for oversized response")
	}
}
