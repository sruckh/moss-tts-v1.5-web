package worker

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
// higgsModel stands in for a non-MOSS model. Goal 03 wires the real Higgs
// RunPod adapter; this goal only needs "some model that is not MOSS-TTS v1.5"
// to exercise the transcript gate.
const higgsModel = "bosonai/higgs-tts-3-4b"

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

	cloneID, err := voiceStore.CreateCloned(context.Background(), userID, "Clone", ".mp3", []byte("ABC"))
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

func (h *harness) enqueueModel(t *testing.T, voiceID int64, text, model string) int64 {
	t.Helper()

	id, err := h.jobs.Enqueue(context.Background(), jobs.NewJob{
		UserID:  h.userID,
		VoiceID: voiceID,
		Text:    text,
		Model:   model,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return id
}

func (h *harness) get(t *testing.T, id int64) jobs.Job {
	t.Helper()

	job, err := h.jobs.Get(context.Background(), id, h.userID)
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
	mu          sync.Mutex
	calls       int
	inputs      []runpod.Input
	higgsInputs []runpod.HiggsInput
	id          string
	status      string
	err         error
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

func (f *fakeSubmitter) SubmitHiggs(_ context.Context, in runpod.HiggsInput) (runpod.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.higgsInputs = append(f.higgsInputs, in)
	if f.err != nil {
		return runpod.Submission{}, f.err
	}
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

func (f *fakeSubmitter) higgsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.higgsInputs)
}

// fakeWhisper stands in for whisper-server. err simulates a sidecar outage or
// a rejected clip; text simulates a successful transcription.
type fakeWhisper struct {
	mu    sync.Mutex
	calls int
	text  string
	err   error
}

func (f *fakeWhisper) Transcribe(_ context.Context, _ []byte, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.text, nil
}

func (f *fakeWhisper) callCount() int {
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

// TestSubmitRoutesHiggsJobToHiggsEndpoint asserts the worker submits a Higgs
// job through SubmitHiggs (the Higgs endpoint) and never touches the MOSS
// Submit path. The clone carries a stored transcript so the transcript gate
// passes and buildHiggsInput can attach the reference.
func TestSubmitRoutesHiggsJobToHiggsEndpoint(t *testing.T) {
	h := newHarness(t)
	if err := h.voices.SetReferenceTranscript(context.Background(), h.cloneID, "hello world"); err != nil {
		t.Fatalf("SetReferenceTranscript: %v", err)
	}
	id := h.enqueueModel(t, h.cloneID, "hello", jobs.HiggsModel)
	client := &fakeSubmitter{id: "higgs-1"}

	h.worker(client, 2).Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusSubmitted {
		t.Errorf("Status = %q, want %s", got.Status, jobs.StatusSubmitted)
	}
	if got.RunPodID != "higgs-1" {
		t.Errorf("RunPodID = %q, want higgs-1", got.RunPodID)
	}
	if client.higgsCallCount() != 1 {
		t.Errorf("SubmitHiggs calls = %d, want 1", client.higgsCallCount())
	}
	if client.callCount() != 1 {
		t.Errorf("total submit calls = %d, want 1", client.callCount())
	}
	if got := client.higgsInputs[0]; got.Text != "hello" {
		t.Errorf("higgs input text = %q, want %q", got.Text, "hello")
	}
	if len(client.higgsInputs[0].References) != 1 {
		t.Errorf("higgs references = %d, want 1", len(client.higgsInputs[0].References))
	}
}

// TestSubmitRoutesMOSSJobToMossEndpoint asserts a default-model job still goes
// through Submit (the MOSS endpoint), confirming the branch does not regress
// the existing path.
func TestSubmitRoutesMOSSJobToMossEndpoint(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, h.stockID, "hello")
	client := &fakeSubmitter{id: "moss-1"}

	h.worker(client, 2).Tick(context.Background())

	got := h.get(t, id)
	if got.RunPodID != "moss-1" {
		t.Errorf("RunPodID = %q, want moss-1", got.RunPodID)
	}
	if client.higgsCallCount() != 0 {
		t.Errorf("SubmitHiggs calls = %d, want 0 for a MOSS job", client.higgsCallCount())
	}
	if client.callCount() != 1 {
		t.Errorf("Submit calls = %d, want 1", client.callCount())
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
			job, err := h.jobs.Get(context.Background(), id, h.userID)
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

// Criterion 4 of Goal 02: MOSS-TTS v1.5 jobs must remain 100% operational
// during a total whisper-server outage.
func TestMOSSJobBypassesWhisperOutage(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, h.cloneID, "hello") // default model -> MOSS-TTS v1.5

	client := &fakeSubmitter{id: "runpod-moss"}
	whisper := &fakeWhisper{err: errors.New("whisper-server unreachable")}
	w := h.worker(client, 2, WithWhisperClient(whisper))
	w.Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusSubmitted {
		t.Fatalf("Status = %q, want %s (err=%q)", got.Status, jobs.StatusSubmitted, got.Error)
	}
	if whisper.callCount() != 0 {
		t.Errorf("whisper calls = %d, want 0 — MOSS jobs must never touch whisper-server", whisper.callCount())
	}
}

// Criterion 3: a Higgs job on a cloned voice with no stored transcript
// triggers one atomic lazy-recovery attempt and then submits.
func TestHiggsJobLazyRecoverySucceeds(t *testing.T) {
	h := newHarness(t)
	id := h.enqueueModel(t, h.cloneID, "hello", higgsModel)

	client := &fakeSubmitter{id: "runpod-higgs"}
	whisper := &fakeWhisper{text: "  This is the reference transcript.  "}
	w := h.worker(client, 2, WithWhisperClient(whisper))
	w.Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusSubmitted {
		t.Fatalf("Status = %q, want %s (err=%q)", got.Status, jobs.StatusSubmitted, got.Error)
	}
	if whisper.callCount() != 1 {
		t.Errorf("whisper calls = %d, want 1", whisper.callCount())
	}

	voice, err := h.voices.Get(context.Background(), h.cloneID)
	if err != nil {
		t.Fatalf("Get voice: %v", err)
	}
	if !voice.ReferenceTranscript.Valid || voice.ReferenceTranscript.V != "This is the reference transcript." {
		t.Errorf("ReferenceTranscript = %+v, want the trimmed whisper text", voice.ReferenceTranscript)
	}
}

// A failed lazy recovery must fail the job outright rather than spend a
// RunPod credit on a job Higgs cannot clone the voice for.
func TestHiggsJobLazyRecoveryFailureFailsJobWithoutSubmitting(t *testing.T) {
	h := newHarness(t)
	id := h.enqueueModel(t, h.cloneID, "hello", higgsModel)

	client := &fakeSubmitter{}
	whisper := &fakeWhisper{err: errors.New("whisper-server: connection refused")}
	w := h.worker(client, 2, WithWhisperClient(whisper))
	w.Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusFailed {
		t.Fatalf("Status = %q, want %s", got.Status, jobs.StatusFailed)
	}
	if got.Error == "" {
		t.Error("failed job recorded no error")
	}
	if got.RunPodID != "" {
		t.Errorf("RunPodID = %q, want empty — a failed transcription must not reach RunPod", got.RunPodID)
	}
	if client.callCount() != 0 {
		t.Errorf("submit calls = %d, want 0", client.callCount())
	}
}

// Empty/whitespace-only speech is a distinct, non-retryable failure reason.
func TestHiggsJobEmptySpeechFailsJob(t *testing.T) {
	h := newHarness(t)
	id := h.enqueueModel(t, h.cloneID, "hello", higgsModel)

	whisper := &fakeWhisper{text: "   "}
	w := h.worker(&fakeSubmitter{}, 2, WithWhisperClient(whisper))
	w.Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusFailed {
		t.Fatalf("Status = %q, want %s", got.Status, jobs.StatusFailed)
	}
	if !strings.Contains(got.Error, "no speech detected") {
		t.Errorf("Error = %q, want it to mention no speech detected", got.Error)
	}
}

// A transcript already on file (e.g. a manual correction) must never be
// silently re-transcribed.
func TestHiggsJobSkipsWhisperWhenTranscriptAlreadySet(t *testing.T) {
	h := newHarness(t)
	if err := h.voices.SetReferenceTranscript(context.Background(), h.cloneID, "Already corrected."); err != nil {
		t.Fatalf("SetReferenceTranscript: %v", err)
	}
	id := h.enqueueModel(t, h.cloneID, "hello", higgsModel)

	whisper := &fakeWhisper{err: errors.New("must not be called")}
	w := h.worker(&fakeSubmitter{id: "runpod-x"}, 2, WithWhisperClient(whisper))
	w.Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusSubmitted {
		t.Fatalf("Status = %q, want %s (err=%q)", got.Status, jobs.StatusSubmitted, got.Error)
	}
	if whisper.callCount() != 0 {
		t.Errorf("whisper calls = %d, want 0 — an existing transcript must not be re-transcribed", whisper.callCount())
	}
}

// A Higgs job against a stock voice carries no reference audio, so there is
// nothing to transcribe.
func TestHiggsJobStockVoiceSkipsTranscriptCheck(t *testing.T) {
	h := newHarness(t)
	id := h.enqueueModel(t, h.stockID, "hello", higgsModel)

	whisper := &fakeWhisper{err: errors.New("must not be called")}
	w := h.worker(&fakeSubmitter{id: "runpod-stock"}, 2, WithWhisperClient(whisper))
	w.Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusSubmitted {
		t.Fatalf("Status = %q, want %s (err=%q)", got.Status, jobs.StatusSubmitted, got.Error)
	}
	if whisper.callCount() != 0 {
		t.Errorf("whisper calls = %d, want 0", whisper.callCount())
	}
}

// Criterion 2: exponential backoff (5s, then 15s) gates each retry, and the
// third attempt spends the WhisperMaxAttempts budget.
func TestTranscriptionClaimBacksOffBetweenAttempts(t *testing.T) {
	h := newHarness(t)
	w := h.worker(&fakeSubmitter{}, 1)
	const voiceID = int64(42)

	if !w.claimTranscription(voiceID) {
		t.Fatal("attempt 1: want claim to succeed immediately")
	}
	if w.claimTranscription(voiceID) {
		t.Fatal("immediate re-claim: want it blocked by the 5s backoff")
	}

	w.leaseMu.Lock()
	w.leases[voiceID].claimedAt = time.Now().Add(-5 * time.Second)
	w.leaseMu.Unlock()
	if !w.claimTranscription(voiceID) {
		t.Fatal("attempt 2: want claim to succeed once the 5s backoff has elapsed")
	}
	if w.claimTranscription(voiceID) {
		t.Fatal("immediate re-claim after attempt 2: want it blocked by the 15s backoff")
	}

	w.leaseMu.Lock()
	w.leases[voiceID].claimedAt = time.Now().Add(-15 * time.Second)
	w.leaseMu.Unlock()
	if !w.claimTranscription(voiceID) {
		t.Fatal("attempt 3: want claim to succeed once the 15s backoff has elapsed")
	}

	if w.claimTranscription(voiceID) {
		t.Fatal("attempt 4: want claim refused — WhisperMaxAttempts is exhausted")
	}
}

// Criterion 2: a claim older than WhisperClaimExpiry (60s) is recoverable
// even if its attempt budget was already spent — the previous holder crashed
// or stalled without releasing it.
func TestTranscriptionStaleClaimRecovery(t *testing.T) {
	h := newHarness(t)
	w := h.worker(&fakeSubmitter{}, 1)
	const voiceID = int64(7)

	w.leaseMu.Lock()
	w.leases[voiceID] = &transcriptionLease{
		claimedAt: time.Now().Add(-90 * time.Second),
		attempts:  WhisperMaxAttempts,
	}
	w.leaseMu.Unlock()

	if !w.claimTranscription(voiceID) {
		t.Fatal("want a claim older than WhisperClaimExpiry to be recoverable")
	}

	w.leaseMu.Lock()
	got := w.leases[voiceID].attempts
	w.leaseMu.Unlock()
	if got != 1 {
		t.Errorf("attempts after stale-claim recovery = %d, want 1 (reset, then re-claimed)", got)
	}
}

// Criterion 2: the 30s (here, shortened) HTTP context timeout must cancel a
// stalled whisper-server request rather than hang the worker.
func TestHTTPWhisperClientEnforcesTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"text":"too late"}`)
	}))
	defer server.Close()

	client := newHTTPWhisperClient(server.URL, 20*time.Millisecond)
	if _, err := client.Transcribe(context.Background(), []byte("audio"), "wav"); err == nil {
		t.Fatal("want a timeout error, got nil")
	}
}

// Criterion 1/2: verifies the exact wire contract from ADR 002 — multipart
// POST /inference with response_format=json and temperature=0.0.
func TestHTTPWhisperClientPostsMultipartInference(t *testing.T) {
	var gotPath, gotMethod, gotFilename, gotResponseFormat, gotTemperature string
	var gotFile []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		defer file.Close()
		gotFile, _ = io.ReadAll(file)
		gotFilename = header.Filename
		gotResponseFormat = r.FormValue("response_format")
		gotTemperature = r.FormValue("temperature")

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":" hello world "}`)
	}))
	defer server.Close()

	client := newHTTPWhisperClient(server.URL, WhisperTimeout)
	text, err := client.Transcribe(context.Background(), []byte("PCM-BYTES"), "wav")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "hello world" {
		t.Errorf("text = %q, want trimmed %q", text, "hello world")
	}
	if gotMethod != http.MethodPost || gotPath != "/inference" {
		t.Errorf("request = %s %s, want POST /inference", gotMethod, gotPath)
	}
	if string(gotFile) != "PCM-BYTES" {
		t.Errorf("uploaded file bytes = %q, want PCM-BYTES", gotFile)
	}
	if gotFilename != "reference.wav" {
		t.Errorf("filename = %q, want reference.wav", gotFilename)
	}
	if gotResponseFormat != "json" || gotTemperature != "0.0" {
		t.Errorf("response_format=%q temperature=%q, want json/0.0", gotResponseFormat, gotTemperature)
	}
}

// The default sidecar URL must match the docker-compose service name — the
// worker has no env var to configure it (see docker-compose.yml).
func TestDefaultWhisperURLMatchesSidecarServiceName(t *testing.T) {
	if DefaultWhisperURL != "http://whisper-server:8080" {
		t.Errorf("DefaultWhisperURL = %q, want http://whisper-server:8080", DefaultWhisperURL)
	}
}

func TestHTTPWhisperClientAlignsVerboseJSON(t *testing.T) {
	var gotPath, gotMethod, gotFilename, gotResponseFormat, gotTokenTimestamps, gotTemperature string
	var gotFile []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		defer file.Close()
		gotFile, _ = io.ReadAll(file)
		gotFilename = header.Filename
		gotResponseFormat = r.FormValue("response_format")
		gotTokenTimestamps = r.FormValue("token_timestamps")
		gotTemperature = r.FormValue("temperature")

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"segments": [
				{
					"words": [
						{"word": " Hello ", "start": 0.1, "end": 0.5},
						{"word": "world ", "start": 0.5, "end": 1.2}
					]
				}
			]
		}`)
	}))
	defer server.Close()

	client := NewHTTPWhisperClient(server.URL, WhisperTimeout)
	wt, err := client.AlignOutput(context.Background(), []byte("PCM-WAV-BYTES"))
	if err != nil {
		t.Fatalf("AlignOutput: %v", err)
	}
	if wt == nil {
		t.Fatal("wt is nil, want non-nil WordTimings")
	}
	if wt.Source != "whisper_cpp" {
		t.Errorf("Source = %q, want whisper_cpp", wt.Source)
	}
	if len(wt.Words) != 2 {
		t.Fatalf("len(Words) = %d, want 2", len(wt.Words))
	}
	if wt.Words[0].W != "Hello" || wt.Words[0].Start != 0.1 || wt.Words[0].End != 0.5 {
		t.Errorf("Words[0] = %+v, want Hello [0.1, 0.5]", wt.Words[0])
	}
	if wt.Words[1].W != "world" || wt.Words[1].Start != 0.5 || wt.Words[1].End != 1.2 {
		t.Errorf("Words[1] = %+v, want world [0.5, 1.2]", wt.Words[1])
	}

	if gotMethod != http.MethodPost || gotPath != "/inference" {
		t.Errorf("request = %s %s, want POST /inference", gotMethod, gotPath)
	}
	if string(gotFile) != "PCM-WAV-BYTES" {
		t.Errorf("uploaded file bytes = %q, want PCM-WAV-BYTES", gotFile)
	}
	if gotFilename != "output.wav" {
		t.Errorf("filename = %q, want output.wav", gotFilename)
	}
	if gotResponseFormat != "verbose_json" || gotTokenTimestamps != "true" || gotTemperature != "0.0" {
		t.Errorf("params = format:%q timestamps:%q temp:%q, want verbose_json/true/0.0", gotResponseFormat, gotTokenTimestamps, gotTemperature)
	}
}

func TestHTTPWhisperClientRejectsInvalidWordTimings(t *testing.T) {
	cases := []struct {
		name     string
		respJSON string
	}{
		{
			name: "negative start timestamp",
			respJSON: `{
				"segments": [{"words": [{"word": "bad", "start": -0.5, "end": 1.0}]}]
			}`,
		},
		{
			name: "reversed interval",
			respJSON: `{
				"segments": [{"words": [{"word": "bad", "start": 1.5, "end": 1.0}]}]
			}`,
		},
		{
			name: "overlapping/non-monotonic intervals",
			respJSON: `{
				"segments": [{"words": [
					{"word": "first", "start": 0.0, "end": 1.0},
					{"word": "second", "start": 0.5, "end": 1.5}
				]}]
			}`,
		},
		{
			name: "non-finite timestamp",
			respJSON: fmt.Sprintf(`{
				"segments": [{"words": [{"word": "bad", "start": %f, "end": 1.0}]}]
			}`, math.NaN()),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.respJSON)
			}))
			defer server.Close()

			client := NewHTTPWhisperClient(server.URL, WhisperTimeout)
			wt, err := client.AlignOutput(context.Background(), []byte("PCM-BYTES"))
			if err == nil && wt != nil {
				t.Errorf("AlignOutput returned success %+v, want error for invalid timing", wt)
			}
		})
	}
}
