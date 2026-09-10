package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/voices"
)

// firstVoiceID returns a seeded stock voice to attach jobs to.
func firstVoiceID(t *testing.T, srv *Server, cookie *http.Cookie) int64 {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/voices", nil)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var items []voices.Voice
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode voices %q: %v", rec.Body.String(), err)
	}
	if len(items) == 0 {
		t.Fatal("no voices seeded")
	}
	return items[0].ID
}

// postJob submits the enqueue form and returns the recorder.
func postJob(t *testing.T, srv *Server, cookie *http.Cookie, form url.Values, accept string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// Criterion 1: POST /jobs authenticated with a valid voice → 200 and a queued row.
func TestCreateJobEnqueuesQueuedRow(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voiceID := firstVoiceID(t, srv, cookie)

	rec := postJob(t, srv, cookie, url.Values{
		"text":     {"Hello from the queue."},
		"voice_id": {strconv.FormatInt(voiceID, 10)},
		"language": {"English"},
	}, "application/json")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	var created jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if created.ID == 0 {
		t.Error("created job has no id")
	}
	if created.Status != jobs.StatusQueued {
		t.Errorf("Status = %q, want %s", created.Status, jobs.StatusQueued)
	}
	if created.VoiceID != voiceID {
		t.Errorf("VoiceID = %d, want %d", created.VoiceID, voiceID)
	}
	if created.Text != "Hello from the queue." {
		t.Errorf("Text = %q", created.Text)
	}
	if created.Language != "English" {
		t.Errorf("Language = %q, want English", created.Language)
	}
	if created.Model != jobs.DefaultModel {
		t.Errorf("Model = %q, want %q", created.Model, jobs.DefaultModel)
	}
	// The browser request must never have reached RunPod.
	if created.RunPodID != "" {
		t.Errorf("RunPodID = %q — the enqueue handler must not submit", created.RunPodID)
	}
}

func TestCreateJobReturnsQueueFragment(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voiceID := firstVoiceID(t, srv, cookie)

	rec := postJob(t, srv, cookie, url.Values{
		"text":     {"Fragment please."},
		"voice_id": {strconv.FormatInt(voiceID, 10)},
	}, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="queue"`) {
		t.Error("response is not the queue fragment")
	}
	if !strings.Contains(body, "Enqueue succeeded") {
		t.Error("response carries no enqueue confirmation")
	}
	if !strings.Contains(body, "Fragment please.") {
		t.Error("queue fragment does not show the new job")
	}
}

func TestCreateJobRequiresAuth(t *testing.T) {
	srv := newTestServer(t)

	rec := postJob(t, srv, nil, url.Values{
		"text":     {"no session"},
		"voice_id": {"1"},
	}, "application/json")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestCreateJobValidation(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voiceID := strconv.FormatInt(firstVoiceID(t, srv, cookie), 10)

	tests := []struct {
		name string
		form url.Values
	}{
		{"empty text", url.Values{"text": {"   "}, "voice_id": {voiceID}}},
		{"no voice", url.Values{"text": {"hi"}}},
		{"unparseable voice", url.Values{"text": {"hi"}, "voice_id": {"abc"}}},
		{"unknown voice", url.Values{"text": {"hi"}, "voice_id": {"99999"}}},
		{"text too long", url.Values{
			"text":     {strings.Repeat("a", jobs.MaxTextRunes+1)},
			"voice_id": {voiceID},
		}},
		{"bad max_new_tokens", url.Values{
			"text": {"hi"}, "voice_id": {voiceID}, "max_new_tokens": {"0"},
		}},
		{"huge max_new_tokens", url.Values{
			"text": {"hi"}, "voice_id": {voiceID}, "max_new_tokens": {"99999999"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJob(t, srv, cookie, tc.form, "application/json")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreateJobEngineSelection(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "blank defaults to MOSS", want: jobs.DefaultModel},
		{name: "whitespace defaults to MOSS", model: "  ", want: jobs.DefaultModel},
		{name: "explicit MOSS", model: jobs.DefaultModel, want: jobs.DefaultModel},
		{name: "explicit Higgs", model: jobs.HiggsModel, want: jobs.HiggsModel},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			cookie := login(t, srv)
			voiceID := firstVoiceID(t, srv, cookie)
			rec := postJob(t, srv, cookie, url.Values{
				"text":     {"Engine selection."},
				"voice_id": {strconv.FormatInt(voiceID, 10)},
				"model":    {tc.model},
			}, "application/json")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			var created jobs.Job
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			if created.Model != tc.want {
				t.Errorf("Model = %q, want %q", created.Model, tc.want)
			}
		})
	}
}

func TestCreateJobRejectsUnknownEngine(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voiceID := firstVoiceID(t, srv, cookie)

	rec := postJob(t, srv, cookie, url.Values{
		"text":     {"Do not queue this."},
		"voice_id": {strconv.FormatInt(voiceID, 10)},
		"model":    {"unknown-engine"},
	}, "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), jobs.ErrModel.Error()) {
		t.Errorf("body = %q, want model validation error", rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	queue := httptest.NewRecorder()
	srv.ServeHTTP(queue, req)
	var items []jobs.Job
	if err := json.Unmarshal(queue.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode queue %q: %v", queue.Body.String(), err)
	}
	if len(items) != 0 {
		t.Fatalf("queue has %d jobs after rejected model, want 0", len(items))
	}
}

func TestJobQueueRouteCapsAtTenRows(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voiceID := firstVoiceID(t, srv, cookie)

	for i := 0; i < queueLimit+2; i++ {
		rec := postJob(t, srv, cookie, url.Values{
			"text":     {"Take " + strconv.Itoa(i)},
			"voice_id": {strconv.FormatInt(voiceID, 10)},
		}, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("enqueue %d status = %d, want 200 (body %q)", i, rec.Code, rec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/jobs/queue", nil)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON queue status = %d, want 200", rec.Code)
	}
	var items []jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode queue %q: %v", rec.Body.String(), err)
	}
	if len(items) != queueLimit {
		t.Fatalf("queue returned %d jobs, want %d", len(items), queueLimit)
	}

	htmlReq := httptest.NewRequest(http.MethodGet, "/jobs/queue", nil)
	htmlReq.AddCookie(cookie)
	htmlRec := httptest.NewRecorder()
	srv.ServeHTTP(htmlRec, htmlReq)
	body := htmlRec.Body.String()
	if !strings.Contains(body, `id="queue"`) {
		t.Error("dedicated route did not return the queue fragment")
	}
	if strings.Contains(body, `id="playback-body"`) || strings.Contains(body, "<audio") {
		t.Error("queue fragment contains player markup")
	}
	if got := strings.Count(body, `id="job-`); got != queueLimit {
		t.Errorf("queue fragment rendered %d rows, want %d", got, queueLimit)
	}
}

func TestCreateJobStoresParams(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voiceID := firstVoiceID(t, srv, cookie)

	rec := postJob(t, srv, cookie, url.Values{
		"text":           {"with params"},
		"voice_id":       {strconv.FormatInt(voiceID, 10)},
		"max_new_tokens": {"1024"},
	}, "application/json")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var created jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := created.Params()["max_new_tokens"]; got != float64(1024) {
		t.Errorf("params max_new_tokens = %v, want 1024", got)
	}
}

func TestQueueIsScopedToTheSession(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voiceID := firstVoiceID(t, srv, cookie)

	postJob(t, srv, cookie, url.Values{
		"text":     {"mine"},
		"voice_id": {strconv.FormatInt(voiceID, 10)},
	}, "application/json")

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var items []jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if len(items) != 1 || items[0].Text != "mine" {
		t.Fatalf("queue = %v, want the one job just enqueued", items)
	}

	// Unauthenticated callers get nothing.
	req = httptest.NewRequest(http.MethodGet, "/jobs", nil)
	req.Header.Set("Accept", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /jobs status = %d, want 401", rec.Code)
	}
}

func TestQueuePageRenders(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/queue", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `hx-post="/jobs"`) {
		t.Error("queue page has no enqueue form")
	}
	if !strings.Contains(body, `id="queue"`) {
		t.Error("queue page has no queue fragment")
	}
}

// /health reports the upstream verdict without dialling out when RunPod is
// unconfigured, and stays behind the session gate.
func TestRunPodHealthRequiresAuthAndReportsConfig(t *testing.T) {
	srv := newTestServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Accept", "application/json")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}

	cookie := login(t, srv)
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		OK     bool `json:"ok"`
		RunPod struct {
			Configured bool   `json:"configured"`
			Reachable  bool   `json:"reachable"`
			Error      string `json:"error"`
		} `json:"runpod"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if !body.OK {
		t.Error("ok = false, want true — the app itself is up")
	}
	if body.RunPod.Configured {
		t.Error("runpod.configured = true, but the test server has no API key")
	}
	if body.RunPod.Error == "" {
		t.Error("unconfigured RunPod reported no reason")
	}
}

// /healthz stays public and independent of RunPod: container liveness must not
// depend on a third party.
func TestHealthzDoesNotDependOnRunPod(t *testing.T) {
	srv := newTestServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a session", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "runpod") {
		t.Error("/healthz leaks upstream state; that belongs on /health")
	}
}

func TestDownloadAudioRoute(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voiceID := firstVoiceID(t, srv, cookie)
	ctx := context.Background()

	// 1. Enqueue job
	id, err := srv.jobs.Enqueue(ctx, jobs.NewJob{
		UserID:  1, // logged in user ID from test setup
		VoiceID: voiceID,
		Text:    "test audio download",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Unauthenticated GET /jobs/{id}/audio -> 401
	req := httptest.NewRequest(http.MethodGet, "/jobs/"+strconv.FormatInt(id, 10)+"/audio", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}

	// Non-ready job GET /jobs/{id}/audio -> 400
	req = httptest.NewRequest(http.MethodGet, "/jobs/"+strconv.FormatInt(id, 10)+"/audio", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("queued job status = %d, want 400", rec.Code)
	}

	// Create dummy audio file and mark ready
	audioFile := filepath.Join(t.TempDir(), "test_job.wav")
	dummyWav := []byte("RIFFxxxxWAVEfmt ")
	if err := os.WriteFile(audioFile, dummyWav, 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := srv.jobs.MarkReady(ctx, id, audioFile, "wav", 24000, 10, 50, ""); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}

	// Authenticated GET /jobs/{id}/audio -> 200 + audio/wav + attachment + bytes
	req = httptest.NewRequest(http.MethodGet, "/jobs/"+strconv.FormatInt(id, 10)+"/audio", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ready job status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "audio/wav") {
		t.Errorf("Content-Type = %q, want audio/wav...", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if string(rec.Body.Bytes()) != string(dummyWav) {
		t.Errorf("body = %q, want %q", rec.Body.String(), dummyWav)
	}
}

func TestJobRoutesDoNotExposeAnotherUsersData(t *testing.T) {
	srv := newTestServer(t)
	cookieA := signInAs(t, srv, "job_owner", "approved")
	cookieB := signInAs(t, srv, "job_other", "approved")
	voiceID := firstVoiceID(t, srv, cookieA)

	createdRec := postJob(t, srv, cookieA, url.Values{
		"text":     {"owner-only render"},
		"voice_id": {strconv.FormatInt(voiceID, 10)},
	}, "application/json")
	if createdRec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200 (body %q)", createdRec.Code, createdRec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(createdRec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode created job %q: %v", createdRec.Body.String(), err)
	}

	dummyWav := []byte("RIFFprivateWAVEfmt ")
	audioFile := filepath.Join(t.TempDir(), "owner-only.wav")
	if err := os.WriteFile(audioFile, dummyWav, 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := srv.jobs.MarkReady(context.Background(), job.ID, audioFile, "wav", 24000, 10, 50, ""); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}

	queueReq := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	queueReq.Header.Set("Accept", "application/json")
	queueReq.AddCookie(cookieB)
	queueRec := httptest.NewRecorder()
	srv.ServeHTTP(queueRec, queueReq)
	if queueRec.Code != http.StatusOK {
		t.Fatalf("other-user queue status = %d, want 200", queueRec.Code)
	}
	var items []jobs.Job
	if err := json.Unmarshal(queueRec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode other-user queue %q: %v", queueRec.Body.String(), err)
	}
	if len(items) != 0 {
		t.Fatalf("other-user queue = %v, want no jobs", items)
	}

	for _, path := range []string{"/", "/queue"} {
		rec := do(t, srv, http.MethodGet, path, cookieB)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), job.Text) {
			t.Errorf("GET %s exposed another user's job text", path)
		}
	}

	playerPath := "/jobs/" + strconv.FormatInt(job.ID, 10) + "/player"
	playerRec := do(t, srv, http.MethodGet, playerPath, cookieB)
	if playerRec.Code != http.StatusNotFound {
		t.Errorf("other-user player status = %d, want 404", playerRec.Code)
	}
	if strings.Contains(playerRec.Body.String(), job.Text) {
		t.Error("other-user player exposed job text")
	}

	audioPath := "/jobs/" + strconv.FormatInt(job.ID, 10) + "/audio"
	audioRec := do(t, srv, http.MethodGet, audioPath, cookieB)
	if audioRec.Code != http.StatusNotFound {
		t.Errorf("other-user audio status = %d, want 404", audioRec.Code)
	}
	if string(audioRec.Body.Bytes()) == string(dummyWav) {
		t.Error("other-user audio returned the WAV bytes")
	}

	deletePath := "/jobs/" + strconv.FormatInt(job.ID, 10)
	deleteRec := do(t, srv, http.MethodDelete, deletePath, cookieB)
	if deleteRec.Code != http.StatusNotFound {
		t.Errorf("other-user delete status = %d, want 404", deleteRec.Code)
	}
	if _, err := srv.jobs.Get(context.Background(), job.ID, job.UserID); err != nil {
		t.Fatalf("job disappeared after wrong-user delete: %v", err)
	}

	ownerRec := do(t, srv, http.MethodGet, audioPath, cookieA)
	if ownerRec.Code != http.StatusOK {
		t.Fatalf("owner audio status = %d, want 200 (body %q)", ownerRec.Code, ownerRec.Body.String())
	}
	if string(ownerRec.Body.Bytes()) != string(dummyWav) {
		t.Errorf("owner audio body = %q, want %q", ownerRec.Body.String(), dummyWav)
	}
}

func TestDeleteJobRoute(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voiceID := firstVoiceID(t, srv, cookie)
	ctx := context.Background()

	// 1. Delete queued job
	queuedID, err := srv.jobs.Enqueue(ctx, jobs.NewJob{
		UserID:  1,
		VoiceID: voiceID,
		Text:    "queued to delete",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/jobs/"+strconv.FormatInt(queuedID, 10), nil)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete queued status = %d, want 200", rec.Code)
	}

	// Verify job is removed from DB
	if _, err := srv.jobs.Get(ctx, queuedID, 1); !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("Get queued job after delete err = %v, want ErrNotFound", err)
	}

	// 2. Delete ready job with audio file
	readyID, err := srv.jobs.Enqueue(ctx, jobs.NewJob{
		UserID:  1,
		VoiceID: voiceID,
		Text:    "ready to delete",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	audioFile := filepath.Join(t.TempDir(), "ready_delete.wav")
	if err := os.WriteFile(audioFile, []byte("data"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := srv.jobs.MarkReady(ctx, readyID, audioFile, "wav", 24000, 10, 50, ""); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}

	req = httptest.NewRequest(http.MethodDelete, "/jobs/"+strconv.FormatInt(readyID, 10), nil)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete ready status = %d, want 200", rec.Code)
	}

	// Verify DB row deleted
	if _, err := srv.jobs.Get(ctx, readyID, 1); !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("Get ready job after delete err = %v, want ErrNotFound", err)
	}

	// Verify audio file deleted from disk
	if _, err := os.Stat(audioFile); !os.IsNotExist(err) {
		t.Errorf("audio file %s still exists after DELETE", audioFile)
	}
}

// TestCreateJobBreezeModes asserts the per-mode contract of the Breeze engine:
// each mode stores the right params_json, and design — the one render that
// legitimately has no voice — enqueues with no voice_id posted and stores a
// NULL link (read back as VoiceID 0), even when the request names a voice.
func TestCreateJobBreezeModes(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voiceID := firstVoiceID(t, srv, cookie)
	voice := strconv.FormatInt(voiceID, 10)

	t.Run("clone", func(t *testing.T) {
		rec := postJob(t, srv, cookie, url.Values{
			"text":     {"clone me"},
			"voice_id": {voice},
			"model":    {jobs.BreezeModel},
			"mode":     {"clone"},
		}, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var created jobs.Job
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if created.Model != jobs.BreezeModel {
			t.Errorf("Model = %q, want %q", created.Model, jobs.BreezeModel)
		}
		if got := created.Params()["mode"]; got != "clone" {
			t.Errorf("params mode = %v, want clone", got)
		}
		if created.VoiceID != voiceID {
			t.Errorf("VoiceID = %d, want %d — clone keeps the voice link", created.VoiceID, voiceID)
		}
	})

	t.Run("clone with cfg_scale", func(t *testing.T) {
		rec := postJob(t, srv, cookie, url.Values{
			"text":      {"scaled"},
			"voice_id":  {voice},
			"model":     {jobs.BreezeModel},
			"mode":      {"clone"},
			"cfg_scale": {"4"},
		}, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var created jobs.Job
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got := created.Params()["cfg_scale"]; got != float64(4) {
			t.Errorf("params cfg_scale = %v, want 4", got)
		}
	})

	t.Run("design posts no voice_id", func(t *testing.T) {
		rec := postJob(t, srv, cookie, url.Values{
			"text":     {"designed voice"},
			"model":    {jobs.BreezeModel},
			"mode":     {"design"},
			"instruct": {"a warm narrator"},
		}, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — design must not require a voice (body %q)", rec.Code, rec.Body.String())
		}
		var created jobs.Job
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if created.VoiceID != 0 {
			t.Errorf("VoiceID = %d, want 0 (SQL NULL) for a design render", created.VoiceID)
		}
		params := created.Params()
		if params["mode"] != "design" || params["instruct"] != "a warm narrator" {
			t.Errorf("params = %v, want mode=design instruct=a warm narrator", params)
		}
	})

	t.Run("design naming a voice still stores no link", func(t *testing.T) {
		// The voice library stays visible in design mode, so this request
		// shape is possible; the render is instruction-only, so the link is
		// deliberately not stored.
		rec := postJob(t, srv, cookie, url.Values{
			"text":     {"designed despite the click"},
			"voice_id": {voice},
			"model":    {jobs.BreezeModel},
			"mode":     {"design"},
			"instruct": {"a warm narrator"},
		}, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var created jobs.Job
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if created.VoiceID != 0 {
			t.Errorf("VoiceID = %d, want 0 — design ignores a named voice", created.VoiceID)
		}
	})

	t.Run("direction", func(t *testing.T) {
		rec := postJob(t, srv, cookie, url.Values{
			"text":     {"directed clone"},
			"voice_id": {voice},
			"model":    {jobs.BreezeModel},
			"mode":     {"direction"},
			"instruct": {"cheerful"},
		}, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var created jobs.Job
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		params := created.Params()
		if params["mode"] != "direction" || params["instruct"] != "cheerful" {
			t.Errorf("params = %v, want mode=direction instruct=cheerful", params)
		}
		if created.VoiceID != voiceID {
			t.Errorf("VoiceID = %d, want %d — direction keeps the voice link", created.VoiceID, voiceID)
		}
	})
}

// TestCreateJobBreezeValidation asserts the Breeze-specific 400s and that none
// of the rejected requests reach the queue.
func TestCreateJobBreezeValidation(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voice := strconv.FormatInt(firstVoiceID(t, srv, cookie), 10)

	tests := []struct {
		name string
		form url.Values
	}{
		{"unknown mode", url.Values{
			"text": {"hi"}, "voice_id": {voice}, "model": {jobs.BreezeModel}, "mode": {"weave"},
		}},
		{"breeze with no mode", url.Values{
			"text": {"hi"}, "voice_id": {voice}, "model": {jobs.BreezeModel},
		}},
		{"design without instruct", url.Values{
			"text": {"hi"}, "model": {jobs.BreezeModel}, "mode": {"design"},
		}},
		{"direction without instruct", url.Values{
			"text": {"hi"}, "voice_id": {voice}, "model": {jobs.BreezeModel}, "mode": {"direction"},
		}},
		{"unparseable cfg_scale", url.Values{
			"text": {"hi"}, "voice_id": {voice}, "model": {jobs.BreezeModel},
			"mode": {"clone"}, "cfg_scale": {"fast"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJob(t, srv, cookie, tc.form, "application/json")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var items []jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode queue %q: %v", rec.Body.String(), err)
	}
	if len(items) != 0 {
		t.Fatalf("queue has %d jobs after rejected Breeze requests, want 0", len(items))
	}
}

// TestHiddenBreezeFieldsDoNotLeakIntoOtherEngines is the D2 regression guard.
// The studio hides the Breeze controls with x-show, which sets display:none —
// the fields still submit. A MOSS or Higgs render therefore posts mode=clone,
// instruct and cfg_scale whether or not anyone touched them. The stored
// params_json (and with it the outbound Extra, which is params_json verbatim)
// must be identical to the same request without those fields.
func TestHiddenBreezeFieldsDoNotLeakIntoOtherEngines(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	voice := strconv.FormatInt(firstVoiceID(t, srv, cookie), 10)

	todayFields := url.Values{
		"text":           {"today's request"},
		"voice_id":       {voice},
		"max_new_tokens": {"1024"},
		"seed":           {"7"},
		"pace":           {"1.5"},
		"pitch":          {"2"},
		"expressiveness": {"0.5"},
		"normalize":      {"on"},
		"output_48k":     {"on"},
	}
	hiddenBreezeFields := url.Values{
		"mode":      {"clone"},
		"instruct":  {"nobody touched this"},
		"cfg_scale": {"4"},
	}

	post := func(model string, extra url.Values) jobs.Job {
		t.Helper()
		form := url.Values{"model": {model}}
		for k, vs := range todayFields {
			form[k] = vs
		}
		for k, vs := range extra {
			form[k] = vs
		}
		rec := postJob(t, srv, cookie, form, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("model %q status = %d, want 200 (body %q)", model, rec.Code, rec.Body.String())
		}
		var created jobs.Job
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return created
	}

	for _, model := range []string{jobs.DefaultModel, jobs.HiggsModel} {
		t.Run(model, func(t *testing.T) {
			clean := post(model, nil)
			dirty := post(model, hiddenBreezeFields)

			if !reflect.DeepEqual(clean.Params(), dirty.Params()) {
				t.Errorf("params differ when hidden Breeze fields are present:\n clean: %v\n dirty: %v",
					clean.Params(), dirty.Params())
			}
			for _, leaked := range []string{"mode", "instruct", "cfg_scale"} {
				if _, ok := dirty.Params()[leaked]; ok {
					t.Errorf("params_json carries %q — Breeze-only fields must not reach the %s worker as Extra", leaked, model)
				}
			}
			// Pin the rest of the shape too, so a future field rename shows
			// up here rather than at the worker.
			if got := clean.Params()["seed"]; got != float64(7) {
				t.Errorf("params seed = %v, want 7", got)
			}
			if got := clean.Params()["normalize"]; got != true {
				t.Errorf("params normalize = %v, want true", got)
			}
		})
	}
}
