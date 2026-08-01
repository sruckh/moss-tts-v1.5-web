package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
