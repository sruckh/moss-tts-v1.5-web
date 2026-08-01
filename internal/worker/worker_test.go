package worker

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sruckh/timbre/internal/db"
	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/runpod"
	"github.com/sruckh/timbre/internal/voices"
)

// harness is a worker wired to a real database and voice store, so the tests
// exercise the actual state transitions rather than a mock's idea of them.
type harness struct {
	jobs    *jobs.Store
	voices  *voices.Store
	userID  int64
	stockID int64
	cloneID int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	handle, err := db.Open(filepath.Join(t.TempDir(), "timbre.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if err := db.Migrate(context.Background(), handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	res, err := handle.Exec(`INSERT INTO users (username, password_hash) VALUES ('tester', 'x')`)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	userID, _ := res.LastInsertId()

	voiceStore := voices.NewStore(handle, t.TempDir())
	if err := voiceStore.SeedStock(context.Background()); err != nil {
		t.Fatalf("SeedStock: %v", err)
	}
	stockID := firstStockID(t, handle)

	cloneID, err := voiceStore.CreateCloned(context.Background(), "Clone", ".mp3", []byte("ABC"))
	if err != nil {
		t.Fatalf("CreateCloned: %v", err)
	}

	return &harness{
		jobs:    jobs.NewStore(handle),
		voices:  voiceStore,
		userID:  userID,
		stockID: stockID,
		cloneID: cloneID,
	}
}

func firstStockID(t *testing.T, handle *sql.DB) int64 {
	t.Helper()

	var id int64
	if err := handle.QueryRow(
		`SELECT id FROM voices WHERE kind = 'stock' ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("find stock voice: %v", err)
	}
	return id
}

func (h *harness) enqueue(t *testing.T, voiceID int64, text string) int64 {
	t.Helper()

	id, err := h.jobs.Enqueue(context.Background(), jobs.NewJob{
		UserID:  h.userID,
		VoiceID: voiceID,
		Text:    text,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return id
}

func (h *harness) get(t *testing.T, id int64) jobs.Job {
	t.Helper()

	job, err := h.jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get job %d: %v", id, err)
	}
	return job
}

func (h *harness) worker(client Submitter, maxInFlight int, opts ...Option) *Worker {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(h.jobs, h.voices, client, maxInFlight, log, opts...)
}

// fakeSubmitter counts calls and records the payloads it was handed. The call
// count is what proves a job is never submitted twice.
type fakeSubmitter struct {
	mu     sync.Mutex
	calls  int
	inputs []runpod.Input
	id     string
	status string
	err    error
}

func (f *fakeSubmitter) Submit(_ context.Context, in runpod.Input) (runpod.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return runpod.Submission{}, f.err
	}
	// Each submission gets a distinct id: jobs.runpod_id is uniquely indexed,
	// so a double would be rejected by the database rather than by the logic
	// under test.
	id := f.id
	if id == "" {
		id = fmt.Sprintf("runpod-%d", f.calls)
	}
	status := f.status
	if status == "" {
		status = runpod.StatusInQueue
	}
	return runpod.Submission{ID: id, Status: status}, nil
}

func (f *fakeSubmitter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Criterion 2: a queued row transitions to submitted and gains a runpod_id.
func TestTickSubmitsQueuedJob(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, h.stockID, "hello")
	client := &fakeSubmitter{id: "runpod-abc"}

	h.worker(client, 2).Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusSubmitted {
		t.Errorf("Status = %q, want %s", got.Status, jobs.StatusSubmitted)
	}
	if got.RunPodID != "runpod-abc" {
		t.Errorf("RunPodID = %q, want runpod-abc", got.RunPodID)
	}
	if client.callCount() != 1 {
		t.Errorf("submit calls = %d, want 1", client.callCount())
	}
}

func TestSubmitMapsInProgressStatus(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, h.stockID, "hello")

	h.worker(&fakeSubmitter{id: "r1", status: runpod.StatusInProgress}, 2).
		Tick(context.Background())

	if got := h.get(t, id); got.Status != jobs.StatusInProgress {
		t.Errorf("Status = %q, want %s", got.Status, jobs.StatusInProgress)
	}
}

// Criterion 3: a row that already has a runpod_id is never submitted again.
func TestWorkerNeverResubmits(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, h.stockID, "hello")
	client := &fakeSubmitter{id: "runpod-abc"}
	w := h.worker(client, 5)

	// Many ticks; the job stays in flight the whole time.
	for range 5 {
		w.Tick(context.Background())
	}

	if client.callCount() != 1 {
		t.Fatalf("submit calls = %d, want exactly 1 across 5 ticks", client.callCount())
	}
	if got := h.get(t, id); got.RunPodID != "runpod-abc" {
		t.Errorf("RunPodID = %q, want the original id", got.RunPodID)
	}
}

// The direct-call guard: handing submit a row that already carries an id must
// not reach RunPod even though ClaimQueued was bypassed.
func TestSubmitIgnoresJobThatAlreadyHasRunPodID(t *testing.T) {
	h := newHarness(t)
	client := &fakeSubmitter{}

	h.worker(client, 1).submit(context.Background(),
		jobs.Job{ID: 1, Text: "hi", RunPodID: "already-there"})

	if client.callCount() != 0 {
		t.Errorf("submit calls = %d, want 0 for a job with a runpod_id", client.callCount())
	}
}

// Criterion 4, unset key: the job fails with a reason instead of hanging.
func TestMissingAPIKeyFailsJob(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, h.stockID, "hello")

	// A real client with no key configured — exactly what an unset
	// RUNPOD_API_KEY produces at boot.
	h.worker(runpod.New("https://api.runpod.ai/v2/x", ""), 2).Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusFailed {
		t.Fatalf("Status = %q, want %s", got.Status, jobs.StatusFailed)
	}
	if got.Error == "" {
		t.Error("failed job recorded no error")
	}
	if got.RunPodID != "" {
		t.Errorf("RunPodID = %q, want empty on a job that never reached RunPod", got.RunPodID)
	}
}

// Criterion 4, invalid key: a 401 is permanent, so the job fails on try one.
func TestInvalidAPIKeyFailsJobImmediately(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, h.stockID, "hello")

	var calls int
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	defer double.Close()

	client := runpod.New(double.URL, "wrong-key", runpod.WithHTTPClient(double.Client()))
	w := h.worker(client, 2)
	w.Tick(context.Background())
	w.Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusFailed {
		t.Fatalf("Status = %q, want %s", got.Status, jobs.StatusFailed)
	}
	if got.Error == "" {
		t.Error("failed job recorded no error")
	}
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1 — a rejected key must not be retried", calls)
	}
}

// A transient failure is retried, then failed once the attempt budget is spent.
func TestTransientFailureRetriesThenFails(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, h.stockID, "hello")

	client := &fakeSubmitter{err: &runpod.Error{StatusCode: http.StatusBadGateway, Body: "upstream down"}}
	w := h.worker(client, 2, WithMaxAttempts(3))

	w.Tick(context.Background())
	if got := h.get(t, id); got.Status != jobs.StatusQueued || got.Attempts != 1 {
		t.Fatalf("after try 1: status=%q attempts=%d, want queued/1", got.Status, got.Attempts)
	}
	w.Tick(context.Background())
	if got := h.get(t, id); got.Status != jobs.StatusQueued || got.Attempts != 2 {
		t.Fatalf("after try 2: status=%q attempts=%d, want queued/2", got.Status, got.Attempts)
	}
	w.Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusFailed {
		t.Errorf("after try 3: status = %q, want %s", got.Status, jobs.StatusFailed)
	}
	if got.Error == "" {
		t.Error("failed job recorded no error")
	}
	if client.callCount() != 3 {
		t.Errorf("submit calls = %d, want 3", client.callCount())
	}
}

func TestMaxInFlightCapsSubmissions(t *testing.T) {
	h := newHarness(t)
	for range 4 {
		h.enqueue(t, h.stockID, "hello")
	}

	client := &fakeSubmitter{}
	w := h.worker(client, 2)

	w.Tick(context.Background())
	if client.callCount() != 2 {
		t.Fatalf("submit calls = %d, want 2 (max-in-flight)", client.callCount())
	}

	// Still saturated: nothing new goes out until something finishes.
	w.Tick(context.Background())
	if client.callCount() != 2 {
		t.Fatalf("submit calls = %d, want still 2 while the budget is full", client.callCount())
	}

	inFlight, err := h.jobs.InFlight(context.Background())
	if err != nil {
		t.Fatalf("InFlight: %v", err)
	}
	if inFlight != 2 {
		t.Errorf("InFlight = %d, want 2", inFlight)
	}
}

// A cloned voice's stored bytes travel base64-inline, with the format the
// handler needs to pick a decoder. Nothing is ever served over HTTP.
func TestClonedVoiceReferenceIsSentInline(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, h.cloneID, "hello")

	client := &fakeSubmitter{}
	h.worker(client, 1).Tick(context.Background())

	if len(client.inputs) != 1 {
		t.Fatalf("got %d submissions, want 1", len(client.inputs))
	}
	got := client.inputs[0]
	if got.ReferenceAudioBase64 != base64.StdEncoding.EncodeToString([]byte("ABC")) {
		t.Errorf("ReferenceAudioBase64 = %q, want the encoded reference bytes",
			got.ReferenceAudioBase64)
	}
	if got.ReferenceFormat != "mp3" {
		t.Errorf("ReferenceFormat = %q, want mp3 (the uploaded extension)", got.ReferenceFormat)
	}
	if got.Stream {
		t.Error("Stream = true, want false")
	}
}

func TestStockVoiceCarriesNoReference(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, h.stockID, "hello")

	client := &fakeSubmitter{}
	h.worker(client, 1).Tick(context.Background())

	if len(client.inputs) != 1 {
		t.Fatalf("got %d submissions, want 1", len(client.inputs))
	}
	if client.inputs[0].ReferenceAudioBase64 != "" {
		t.Error("stock voice submission carried reference audio")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, h.stockID, "hello")

	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeSubmitter{id: "runpod-abc"}
	w := h.worker(client, 1)

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// Run drains once immediately, so the job moves without waiting a tick.
	<-waitForStatus(t, h, id, jobs.StatusSubmitted)
	cancel()
	<-done
}

// waitForStatus polls the job until it reaches want, failing the test on
// timeout. It returns a channel closed once the status matches.
func waitForStatus(t *testing.T, h *harness, id int64, want string) <-chan struct{} {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)

		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			job, err := h.jobs.Get(context.Background(), id)
			switch {
			case err == nil && job.Status == want:
				return
			case errors.Is(err, jobs.ErrNotFound):
				t.Errorf("job %d disappeared", id)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("job %d never reached status %q", id, want)
	}()
	return done
}
