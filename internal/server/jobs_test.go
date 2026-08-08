package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
