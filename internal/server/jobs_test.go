package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/runpod"
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

func postMultipartJob(t *testing.T, srv *Server, cookie *http.Cookie, fields map[string]string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("write field %s: %v", key, err)
		}
	}
	for field, data := range files {
		part, err := writer.CreateFormFile(field, field+".wav")
		if err != nil {
			t.Fatalf("create file field %s: %v", field, err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatalf("write file field %s: %v", field, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/jobs", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
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

func baseAuKFields(task string) map[string]string {
	return map[string]string{
		"model": jobs.AuKModel, "task": task,
		"instruction": "perform the requested task", "model_variant": runpod.AuKVariantFlash,
		"nfe": "4", "cfg_scale": "0", "response_delivery": runpod.AuKDeliveryBase64,
		"seed": "7",
	}
}

func createAuKClone(t *testing.T, srv *Server) int64 {
	t.Helper()
	id, err := srv.voices.CreateCloned(context.Background(), 1, "AuK reference", ".wav", []byte("RIFF-reference"))
	if err != nil {
		t.Fatalf("CreateCloned: %v", err)
	}
	if err := srv.voices.SetReferenceTranscript(context.Background(), id, "reference words"); err != nil {
		t.Fatalf("SetReferenceTranscript: %v", err)
	}
	return id
}

func createReadyRender(t *testing.T, srv *Server, userID, voiceID int64, data []byte) jobs.Job {
	t.Helper()
	id, err := srv.jobs.Enqueue(context.Background(), jobs.NewJob{
		UserID: userID, VoiceID: voiceID, Text: "ready source", Model: jobs.DefaultModel,
	})
	if err != nil {
		t.Fatalf("Enqueue source render: %v", err)
	}
	path := filepath.Join(srv.cfg.AudioDir, "source-"+strconv.FormatInt(id, 10)+".wav")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatalf("write source render: %v", err)
	}
	if err := srv.jobs.MarkReady(context.Background(), id, path, "wav", 24000, 0, 0, ""); err != nil {
		t.Fatalf("MarkReady source render: %v", err)
	}
	job, err := srv.jobs.Get(context.Background(), id, userID)
	if err != nil {
		t.Fatalf("Get source render: %v", err)
	}
	return job
}

func TestCreateJobAuKTaskMatrix(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	stockID := firstVoiceID(t, srv, cookie)
	cloneID := createAuKClone(t, srv)
	valid := []struct {
		name      string
		task      string
		voiceID   int64
		files     map[string][]byte
		wantVoice int64
	}{
		{"instruct", runpod.AuKTaskInstructTTS, 0, nil, 0},
		{"zero shot", runpod.AuKTaskZeroShotTTS, cloneID, nil, cloneID},
		{"content edit", runpod.AuKTaskContentEdit, 0, map[string][]byte{"audio_file": []byte("RIFF-source")}, 0},
		{"acoustic edit", runpod.AuKTaskAcousticEdit, 0, map[string][]byte{"audio_file": []byte("RIFF-source")}, 0},
		{"paralinguistic edit", runpod.AuKTaskParalinguisticEdit, 0, map[string][]byte{"audio_file": []byte("RIFF-source")}, 0},
		{"enhancement", runpod.AuKTaskEnhancement, 0, map[string][]byte{"audio_file": []byte("RIFF-source")}, 0},
		{"separation", runpod.AuKTaskSeparation, 0, map[string][]byte{"audio_file": []byte("RIFF-source")}, 0},
		{"auto instruct from stock", runpod.AuKTaskAuto, stockID, nil, 0},
		{"auto zero shot from clone", runpod.AuKTaskAuto, cloneID, nil, cloneID},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			fields := baseAuKFields(tc.task)
			if tc.voiceID > 0 {
				fields["voice_id"] = strconv.FormatInt(tc.voiceID, 10)
			}
			rec := postMultipartJob(t, srv, cookie, fields, tc.files)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
			}
			var job jobs.Job
			if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
				t.Fatalf("decode job: %v", err)
			}
			if job.Model != jobs.AuKModel || job.VoiceID != tc.wantVoice || job.Text != fields["instruction"] {
				t.Fatalf("job = %+v, want voice %d", job, tc.wantVoice)
			}
			params := job.Params()
			if params["task"] != tc.task || params["model_variant"] != runpod.AuKVariantFlash {
				t.Fatalf("params = %#v", params)
			}
			if tc.wantVoice > 0 {
				if _, ok := params["prompt_audio_path"].(string); !ok {
					t.Fatalf("selected clone was not copied into params: %#v", params)
				}
				if params["prompt_text"] != "reference words" {
					t.Fatalf("stored transcript not reused: %#v", params)
				}
			}
			// Flash coerces cfg_scale to 0 and pins nfe 4: the persisted job
			// records what the render will actually use.
			if params["cfg_scale"] != float64(0) || params["nfe"] != float64(4) {
				t.Fatalf("flash defaults = %#v", params)
			}
		})
	}
}

func TestCreateJobAuKValidation(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	stockID := firstVoiceID(t, srv, cookie)
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"missing instruction", func(f map[string]string) { delete(f, "instruction") }},
		{"zero shot missing voice", func(f map[string]string) { f["task"] = runpod.AuKTaskZeroShotTTS }},
		{"zero shot stock voice", func(f map[string]string) {
			f["task"] = runpod.AuKTaskZeroShotTTS
			f["voice_id"] = strconv.FormatInt(stockID, 10)
		}},
		{"instruct direct source forbidden", func(f map[string]string) { f["audio"] = "YQ==" }},
		{"edit missing source", func(f map[string]string) { f["task"] = runpod.AuKTaskContentEdit }},
		{"edit prompt upload forbidden", func(f map[string]string) { f["task"] = runpod.AuKTaskContentEdit; f["prompt_audio"] = "YQ==" }},
		{"auto direct source forbidden", func(f map[string]string) { f["task"] = runpod.AuKTaskAuto; f["audio"] = "YQ==" }},
		{"prompt text alone", func(f map[string]string) { f["prompt_text"] = "orphan" }},
		{"legacy prompt base64", func(f map[string]string) { f["task"] = runpod.AuKTaskZeroShotTTS; f["prompt_audio"] = "YQ==" }},
		{"legacy prompt URL", func(f map[string]string) {
			f["task"] = runpod.AuKTaskZeroShotTTS
			f["prompt_audio"] = "https://example.test/ref.wav"
		}},
		{"bad task", func(f map[string]string) { f["task"] = "weave" }},
		{"flash nfe", func(f map[string]string) { f["nfe"] = "9" }},
		{"base nfe", func(f map[string]string) {
			f["model_variant"] = runpod.AuKVariantBase
			f["nfe"] = "15"
			f["cfg_scale"] = "2"
		}},
		{"base cfg", func(f map[string]string) {
			f["model_variant"] = runpod.AuKVariantBase
			f["nfe"] = "32"
			f["cfg_scale"] = "6"
		}},
		{"duration", func(f map[string]string) { f["gen_seconds"] = "301" }},
		{"delivery", func(f map[string]string) { f["response_delivery"] = "mail" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fields := baseAuKFields(runpod.AuKTaskInstructTTS)
			tc.mutate(fields)
			rec := postMultipartJob(t, srv, cookie, fields, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreateJobAuKUploadPersistsPrivately(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	fields := baseAuKFields(runpod.AuKTaskContentEdit)
	rec := postMultipartJob(t, srv, cookie, fields, map[string][]byte{"audio_file": []byte("RIFFtest")})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	paths := job.InputPaths()
	if len(paths) != 1 {
		t.Fatalf("input paths = %v", paths)
	}
	if !strings.HasPrefix(paths[0], filepath.Join(srv.cfg.AudioDir, "inputs")) {
		t.Fatalf("input path %q is outside private input directory", paths[0])
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatalf("stored input: %v", err)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/jobs/"+strconv.FormatInt(job.ID, 10), nil)
	deleteReq.AddCookie(cookie)
	deleteReq.Header.Set("Accept", "application/json")
	deleteRec := httptest.NewRecorder()
	srv.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", deleteRec.Code)
	}
	if _, err := os.Stat(paths[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("input remains after delete: %v", err)
	}
}

func TestCreateJobAuKSelectedRenderIsCopiedPrivately(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	stockID := firstVoiceID(t, srv, cookie)
	source := createReadyRender(t, srv, 1, stockID, []byte("RIFF-render-source"))
	fields := baseAuKFields(runpod.AuKTaskContentEdit)
	fields["source_job_id"] = strconv.FormatInt(source.ID, 10)
	rec := postMultipartJob(t, srv, cookie, fields, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	params := job.Params()
	if params["source_job_id"] != float64(source.ID) {
		t.Fatalf("source provenance = %#v", params)
	}
	copied, ok := params["audio_path"].(string)
	if !ok || copied == source.AudioPath {
		t.Fatalf("source was not copied privately: %#v", params)
	}
	data, err := os.ReadFile(copied)
	if err != nil || string(data) != "RIFF-render-source" {
		t.Fatalf("copied source = %q, err=%v", data, err)
	}
}

func TestCreateJobAuKRejectsInvalidSourceSelections(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	stockID := firstVoiceID(t, srv, cookie)
	ready := createReadyRender(t, srv, 1, stockID, []byte("RIFF-ready"))
	queuedID, err := srv.jobs.Enqueue(context.Background(), jobs.NewJob{
		UserID: 1, VoiceID: stockID, Text: "not ready", Model: jobs.DefaultModel,
	})
	if err != nil {
		t.Fatalf("enqueue pending source: %v", err)
	}

	tests := []struct {
		name   string
		fields map[string]string
		files  map[string][]byte
	}{
		{"missing render", map[string]string{"source_job_id": "999999"}, nil},
		{"render not ready", map[string]string{"source_job_id": strconv.FormatInt(queuedID, 10)}, nil},
		{"upload and render", map[string]string{"source_job_id": strconv.FormatInt(ready.ID, 10)}, map[string][]byte{"audio_file": []byte("RIFF-upload")}},
		{"legacy direct source", map[string]string{"audio": "YQ=="}, nil},
		{"legacy prompt upload", nil, map[string][]byte{"prompt_audio_file": []byte("RIFF-prompt")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fields := baseAuKFields(runpod.AuKTaskContentEdit)
			for key, value := range tc.fields {
				fields[key] = value
			}
			rec := postMultipartJob(t, srv, cookie, fields, tc.files)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreateJobAuKRejectsAnotherUsersRender(t *testing.T) {
	srv := newTestServer(t)
	ownerCookie := signInAs(t, srv, "source_owner", auth.StatusApproved)
	otherCookie := signInAs(t, srv, "source_other", auth.StatusApproved)
	ownerID := userIDByName(t, srv, "source_owner")
	stockID := firstVoiceID(t, srv, ownerCookie)
	source := createReadyRender(t, srv, ownerID, stockID, []byte("RIFF-private"))

	fields := baseAuKFields(runpod.AuKTaskContentEdit)
	fields["source_job_id"] = strconv.FormatInt(source.ID, 10)
	rec := postMultipartJob(t, srv, otherCookie, fields, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without revealing the foreign render (body %q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "RIFF-private") {
		t.Fatal("foreign render bytes leaked in the rejection")
	}
}

func TestCreateJobAuKURLEncodedWithoutFiles(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	form := url.Values{
		"model": {jobs.AuKModel}, "task": {runpod.AuKTaskInstructTTS},
		"instruction": {"urlencoded api request"}, "model_variant": {runpod.AuKVariantFlash},
		"nfe": {"4"}, "cfg_scale": {"0"}, "response_delivery": {runpod.AuKDeliveryBase64},
	}
	rec := postJob(t, srv, cookie, form, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.Model != jobs.AuKModel || job.Text != "urlencoded api request" {
		t.Fatalf("job = %+v", job)
	}
}
