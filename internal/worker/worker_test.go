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
	"os"
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
	inputs       []runpod.Input
	higgsInputs  []runpod.HiggsInput
	breezeInputs []runpod.BreezeInput
	aukInputs    []runpod.AuKInput
	id           string
	status       string
	err          error
}

func (f *fakeSubmitter) SubmitBreeze(_ context.Context, in runpod.BreezeInput) (runpod.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.breezeInputs = append(f.breezeInputs, in)
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


func (f *fakeSubmitter) SubmitAuK(_ context.Context, in runpod.AuKInput) (runpod.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.aukInputs = append(f.aukInputs, in)
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

// enqueueFull enqueues a job with an explicit model and params map — the shape
// handleCreateJob produces after parseJobParams.
func (h *harness) enqueueFull(t *testing.T, voiceID int64, text, model string, params map[string]any) int64 {
	t.Helper()

	id, err := h.jobs.Enqueue(context.Background(), jobs.NewJob{
		UserID:  h.userID,
		VoiceID: voiceID,
		Text:    text,
		Model:   model,
		Params:  params,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return id
}

// TestSubmitRoutesBreezeJobToBreezeEndpoint asserts the worker submits a Breeze
// job through SubmitBreeze and never touches the MOSS or Higgs paths, and that
// the captured BreezeInput carries the clone reference and its transcript. The
// clone carries a stored transcript so the transcript gate passes.
func TestSubmitRoutesBreezeJobToBreezeEndpoint(t *testing.T) {
	h := newHarness(t)
	if err := h.voices.SetReferenceTranscript(context.Background(), h.cloneID, "hello world"); err != nil {
		t.Fatalf("SetReferenceTranscript: %v", err)
	}
	id := h.enqueueFull(t, h.cloneID, "hello", jobs.BreezeModel, map[string]any{"mode": "clone"})
	client := &fakeSubmitter{id: "breeze-1"}

	h.worker(client, 2).Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusSubmitted {
		t.Errorf("Status = %q, want %s", got.Status, jobs.StatusSubmitted)
	}
	if got.RunPodID != "breeze-1" {
		t.Errorf("RunPodID = %q, want breeze-1", got.RunPodID)
	}
	if client.callCount() != 1 {
		t.Errorf("total submit calls = %d, want 1", client.callCount())
	}
	if len(client.inputs) != 0 || len(client.higgsInputs) != 0 {
		t.Errorf("MOSS/Higgs paths touched: inputs=%d higgsInputs=%d, want 0/0",
			len(client.inputs), len(client.higgsInputs))
	}
	if len(client.breezeInputs) != 1 {
		t.Fatalf("breezeInputs = %d, want 1", len(client.breezeInputs))
	}
	in := client.breezeInputs[0]
	if in.Text != "hello" {
		t.Errorf("breeze input text = %q, want %q", in.Text, "hello")
	}
	if in.Mode != runpod.BreezeModeClone {
		t.Errorf("breeze input mode = %q, want %q", in.Mode, runpod.BreezeModeClone)
	}
	if len(in.References) != 1 {
		t.Errorf("breeze references = %d, want 1", len(in.References))
	}
	if in.ReferenceText != "hello world" {
		t.Errorf("breeze reference text = %q, want the stored transcript", in.ReferenceText)
	}
	if in.Instruct != "" {
		t.Errorf("breeze instruct = %q, want empty for clone mode", in.Instruct)
	}
}

// TestBreezeDirectionCarriesInstruction asserts direction mode attaches the
// clone reference and transcript like clone mode, plus the instruction.
func TestBreezeDirectionCarriesInstruction(t *testing.T) {
	h := newHarness(t)
	if err := h.voices.SetReferenceTranscript(context.Background(), h.cloneID, "hello world"); err != nil {
		t.Fatalf("SetReferenceTranscript: %v", err)
	}
	id := h.enqueueFull(t, h.cloneID, "hello", jobs.BreezeModel,
		map[string]any{"mode": "direction", "instruct": "cheerful", "cfg_scale": 4.0})
	client := &fakeSubmitter{}

	h.worker(client, 2).Tick(context.Background())

	if got := h.get(t, id); got.Status != jobs.StatusSubmitted {
		t.Fatalf("Status = %q, want %s", got.Status, jobs.StatusSubmitted)
	}
	if len(client.breezeInputs) != 1 {
		t.Fatalf("breezeInputs = %d, want 1", len(client.breezeInputs))
	}
	in := client.breezeInputs[0]
	if in.Mode != runpod.BreezeModeDirection {
		t.Errorf("mode = %q, want %q", in.Mode, runpod.BreezeModeDirection)
	}
	if in.Instruct != "cheerful" {
		t.Errorf("instruct = %q, want %q", in.Instruct, "cheerful")
	}
	if in.CfgScale != 4.0 {
		t.Errorf("cfg_scale = %v, want 4.0", in.CfgScale)
	}
	if len(in.References) != 1 || in.ReferenceText != "hello world" {
		t.Errorf("references = %d, text = %q — direction keeps the clone reference",
			len(in.References), in.ReferenceText)
	}
}

// TestBreezeDesignCarriesNoReference asserts design mode renders from the
// instruction alone: no voice is enqueued, the transcript gate is bypassed,
// and the BreezeInput carries no reference even when the job somehow names a
// voice (the compose card's library stays visible, so a design request naming
// a voice is possible and must be ignored, not honored).
func TestBreezeDesignCarriesNoReference(t *testing.T) {
	h := newHarness(t)
	params := map[string]any{"mode": "design", "instruct": "a warm narrator", "cfg_scale": 4.0}

	for _, tc := range []struct {
		name    string
		voiceID int64
	}{
		{"no voice", 0},
		{"names a voice anyway", h.cloneID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := h.enqueueFull(t, tc.voiceID, "hello", jobs.BreezeModel, params)
			// Distinct ids per subtest: jobs.runpod_id is uniquely indexed.
			client := &fakeSubmitter{id: "breeze-design-" + tc.name}

			h.worker(client, 2).Tick(context.Background())

			if got := h.get(t, id); got.Status != jobs.StatusSubmitted {
				t.Fatalf("Status = %q, want %s (error %q)", got.Status, jobs.StatusSubmitted, got.Error)
			}
			if len(client.breezeInputs) != 1 {
				t.Fatalf("breezeInputs = %d, want 1", len(client.breezeInputs))
			}
			in := client.breezeInputs[0]
			if in.Mode != runpod.BreezeModeDesign {
				t.Errorf("mode = %q, want %q", in.Mode, runpod.BreezeModeDesign)
			}
			if in.Instruct != "a warm narrator" {
				t.Errorf("instruct = %q, want %q", in.Instruct, "a warm narrator")
			}
			if len(in.References) != 0 || in.ReferenceText != "" {
				t.Errorf("design carried reference audio %d clips / text %q, want none — the worker rejects one outright",
					len(in.References), in.ReferenceText)
			}
		})
	}
}

// TestBuildBreezeInputValidationErrors pins the builder's own defenses against
// jobs that should never have been enqueued: the handler 400s these, but a row
// written by any other path must still fail here rather than reach RunPod.
// TestBuildBreezeInputValidationErrors pins the builder's own defenses against
// jobs that should never have been enqueued: the handler 400s these, but a row
// written by any other path must still fail here rather than reach RunPod. The
// jobs are constructed directly because the store's Enqueue validation rightly
// refuses several of these shapes (a clone render with no voice, say).
func TestBuildBreezeInputValidationErrors(t *testing.T) {
	h := newHarness(t)
	if err := h.voices.SetReferenceTranscript(context.Background(), h.cloneID, "hello world"); err != nil {
		t.Fatalf("SetReferenceTranscript: %v", err)
	}
	w := h.worker(&fakeSubmitter{}, 1)

	jobWith := func(voiceID int64, paramsJSON string) jobs.Job {
		return jobs.Job{UserID: h.userID, VoiceID: voiceID, Text: "hi", Model: jobs.BreezeModel, ParamsJSON: paramsJSON}
	}

	tests := []struct {
		name    string
		job     jobs.Job
		wantErr string
	}{
		{"missing mode", jobWith(h.cloneID, ""), "explicit mode"},
		{"unknown mode", jobWith(h.cloneID, `{"mode":"weave"}`), `unknown breeze mode "weave"`},
		{"design without instruct", jobWith(0, `{"mode":"design"}`), "design mode requires an instruction"},
		{"direction without instruct", jobWith(h.cloneID, `{"mode":"direction"}`), "direction mode requires an instruction"},
		{"clone without voice", jobWith(0, `{"mode":"clone"}`), "requires a cloned reference voice"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.buildBreezeInput(context.Background(), tc.job)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestBuildBreezeInputRequiresTranscript asserts clone and direction fail when
// the named voice has no stored transcript. Uses a second clone with no
// transcript so the case is the builder's check, not the lazy-recovery gate.
func TestBuildBreezeInputRequiresTranscript(t *testing.T) {
	h := newHarness(t)
	w := h.worker(&fakeSubmitter{}, 1)

	for _, mode := range []string{runpod.BreezeModeClone, runpod.BreezeModeDirection} {
		t.Run(mode, func(t *testing.T) {
			params := map[string]any{"mode": mode}
			if mode == runpod.BreezeModeDirection {
				params["instruct"] = "brisk"
			}
			id := h.enqueueFull(t, h.cloneID, "hi", jobs.BreezeModel, params)
			_, err := w.buildBreezeInput(context.Background(), h.get(t, id))
			if err == nil || !strings.Contains(err.Error(), "no reference transcript") {
				t.Errorf("err = %v, want a missing-transcript error", err)
			}
		})
	}
}

// TestBuildBreezeInputRejectsOversizeReference asserts the decoded-byte cap is
// enforced before anything is encoded or sent, surfacing the runpod package's
// own validation error.
func TestBuildBreezeInputRejectsOversizeReference(t *testing.T) {
	h := newHarness(t)
	oversize := make([]byte, (4<<20)+1)
	voiceID, err := h.voices.CreateCloned(context.Background(), h.userID, "Big", ".wav", oversize)
	if err != nil {
		t.Fatalf("CreateCloned: %v", err)
	}
	if err := h.voices.SetReferenceTranscript(context.Background(), voiceID, "words"); err != nil {
		t.Fatalf("SetReferenceTranscript: %v", err)
	}
	id := h.enqueueFull(t, voiceID, "hi", jobs.BreezeModel, map[string]any{"mode": "clone"})

	w := h.worker(&fakeSubmitter{}, 1)
	_, err = w.buildBreezeInput(context.Background(), h.get(t, id))

	var valErr *runpod.BreezeValidationError
	if !errors.As(err, &valErr) {
		t.Errorf("err = %v, want a *runpod.BreezeValidationError", err)
	}
}

// TestBuildInputExtraCarriesStoredParamsVerbatim is the outbound half of the
// D2 regression contract: buildInput forwards params_json as Extra unchanged,
// so a MOSS or Higgs row stored without Breeze fields (the server strips them)
// reaches the worker as exactly today's payload — no mode, instruct, or
// cfg_scale can appear unless the row itself carries them.
func TestBuildInputExtraCarriesStoredParamsVerbatim(t *testing.T) {
	h := newHarness(t)
	h.enqueueFull(t, h.stockID, "hi", jobs.DefaultModel,
		map[string]any{"seed": 7, "pace": 1.5})
	client := &fakeSubmitter{}

	h.worker(client, 2).Tick(context.Background())

	if len(client.inputs) != 1 {
		t.Fatalf("inputs = %d, want 1", len(client.inputs))
	}
	extra := client.inputs[0].Extra
	if len(extra) != 2 {
		t.Fatalf("Extra = %v, want exactly the two stored params", extra)
	}
	if extra["seed"] != float64(7) || extra["pace"] != 1.5 {
		t.Errorf("Extra = %v, want seed=7 pace=1.5", extra)
	}
	for _, leaked := range []string{"mode", "instruct", "cfg_scale"} {
		if _, ok := extra[leaked]; ok {
			t.Errorf("Extra carries %q — Breeze-only fields must never reach the MOSS worker", leaked)
		}
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
			name: "non-monotonic backwards jump",
			respJSON: `{
				"segments": [{"words": [
					{"word": "first", "start": 5.0, "end": 6.0},
					{"word": "second", "start": 1.0, "end": 2.0}
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


func TestSubmitRoutesAuKJobAndRemovesPrivateInput(t *testing.T) {
	h := newHarness(t)
	inputPath := filepath.Join(t.TempDir(), "source.wav")
	if err := os.WriteFile(inputPath, []byte("RIFF-AUK"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := h.enqueueFull(t, 0, "enhance this speech", jobs.AuKModel, map[string]any{
		"task": runpod.AuKTaskEnhancement, "audio_path": inputPath,
		"model_variant": runpod.AuKVariantFlash, "nfe": 4,
		"cfg_scale": 0, "response_delivery": runpod.AuKDeliveryBase64,
	})
	client := &fakeSubmitter{id: "auk-1"}
	h.worker(client, 2).Tick(context.Background())

	got := h.get(t, id)
	if got.Status != jobs.StatusSubmitted || got.RunPodID != "auk-1" {
		t.Fatalf("job = %+v", got)
	}
	if len(client.aukInputs) != 1 || len(client.inputs) != 0 || len(client.higgsInputs) != 0 || len(client.breezeInputs) != 0 {
		t.Fatalf("routes: auk=%d moss=%d higgs=%d breeze=%d", len(client.aukInputs), len(client.inputs), len(client.higgsInputs), len(client.breezeInputs))
	}
	in := client.aukInputs[0]
	if in.Task != runpod.AuKTaskEnhancement || in.Audio != base64.StdEncoding.EncodeToString([]byte("RIFF-AUK")) {
		t.Fatalf("AuK input = %+v", in)
	}
	if _, err := os.Stat(inputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("submitted input was not removed: %v", err)
	}
}

func TestAuKTransientSubmitFailureRetainsInputForRetry(t *testing.T) {
	h := newHarness(t)
	inputPath := filepath.Join(t.TempDir(), "source.wav")
	if err := os.WriteFile(inputPath, []byte("RIFF-AUK"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := h.enqueueFull(t, 0, "edit this", jobs.AuKModel, map[string]any{
		"task": runpod.AuKTaskContentEdit, "audio_path": inputPath,
		"model_variant": runpod.AuKVariantFlash, "nfe": 4,
		"cfg_scale": 0, "response_delivery": runpod.AuKDeliveryBase64,
	})
	client := &fakeSubmitter{err: &runpod.Error{StatusCode: http.StatusServiceUnavailable}}
	h.worker(client, 2).Tick(context.Background())
	got := h.get(t, id)
	if got.Status != jobs.StatusQueued || got.Attempts != 1 {
		t.Fatalf("job = %+v", got)
	}
	if _, err := os.Stat(inputPath); err != nil {
		t.Fatalf("retry input was removed: %v", err)
	}
}

func TestBuildAuKInputPreservesURLAndParameters(t *testing.T) {
	h := newHarness(t)
	job := jobs.Job{Text: "say hello", Model: jobs.AuKModel, ParamsJSON: `{"task":"zero_shot_tts","prompt_audio":"https://example.test/prompt.wav","prompt_text":"hello","gen_seconds":2.5,"gen_text":"hello","model_variant":"base","nfe":32,"cfg_scale":2.5,"seed":99,"response_delivery":"s3"}`}
	in, err := h.worker(&fakeSubmitter{}, 1).buildAuKInput(job)
	if err != nil {
		t.Fatalf("buildAuKInput: %v", err)
	}
	if in.PromptAudio != "https://example.test/prompt.wav" || in.PromptText != "hello" || in.ModelVariant != runpod.AuKVariantBase || in.NFE != 32 || in.CfgScale != 2.5 || in.Seed == nil || *in.Seed != 99 || in.ResponseDelivery != runpod.AuKDeliveryS3 {
		t.Fatalf("AuK input = %+v", in)
	}
}
