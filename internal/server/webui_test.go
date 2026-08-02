package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/voices"
)

// uploadClone stores a cloned voice through the real upload route and returns
// its id together with the bytes on disk, so a preview can be compared against
// what was actually uploaded.
func uploadClone(t *testing.T, srv *Server, cookie *http.Cookie, filename string, data []byte) int64 {
	t.Helper()

	req := newUploadRequest(t, "reference", filename, data)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return clonedID(t, srv)
}

// voiceName reads a voice's current name straight from the store.
func voiceName(t *testing.T, srv *Server, id int64) string {
	t.Helper()
	v, err := srv.voices.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("voices.Get(%d): %v", id, err)
	}
	return v.Name
}

// postForm submits an urlencoded form to path, optionally signed in.
func postForm(t *testing.T, srv *Server, cookie *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// get issues an authenticated (or anonymous) GET.
func get(t *testing.T, srv *Server, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("HX-Request", "true")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// A cloned voice arrives named after its file. Renaming it is the point.
func TestVoiceRenameUpdatesTheName(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	id := uploadClone(t, srv, cookie, "take-01-final-FINAL.wav", []byte("reference-bytes"))

	rec := postForm(t, srv, cookie, "/voices/"+strconv.FormatInt(id, 10)+"/name",
		url.Values{"name": {"Narrator"}})

	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := voiceName(t, srv, id); got != "Narrator" {
		t.Errorf("name = %q, want Narrator", got)
	}
	// The response is the refreshed grid, so HTMX can swap the library in place.
	if !strings.Contains(rec.Body.String(), "Narrator") {
		t.Errorf("rename response is not the refreshed grid: %s", rec.Body.String())
	}
}

func TestVoiceRenameRejectsEmptyName(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	id := uploadClone(t, srv, cookie, "clip.wav", []byte("reference-bytes"))
	before := voiceName(t, srv, id)

	rec := postForm(t, srv, cookie, "/voices/"+strconv.FormatInt(id, 10)+"/name",
		url.Values{"name": {"   "}})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := voiceName(t, srv, id); got != before {
		t.Errorf("name changed to %q despite the rejection", got)
	}
}

// A stock voice is reconciled by name on every boot, so renaming one would lose
// it (and every job's link to it) on the next restart. The route says no.
func TestVoiceRenameRefusesStockVoice(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	stockID := firstVoiceID(t, srv, cookie)

	rec := postForm(t, srv, cookie, "/voices/"+strconv.FormatInt(stockID, 10)+"/name",
		url.Values{"name": {"Renamed stock"}})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := voiceName(t, srv, stockID); got != "Moss" {
		t.Errorf("stock voice was renamed to %q", got)
	}
}

func TestVoiceRenameUnauthenticatedRejected(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	id := uploadClone(t, srv, cookie, "clip.wav", []byte("reference-bytes"))

	rec := postForm(t, srv, nil, "/voices/"+strconv.FormatInt(id, 10)+"/name",
		url.Values{"name": {"Intruder"}})

	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 401 or 302", rec.Code)
	}
	if got := voiceName(t, srv, id); got == "Intruder" {
		t.Error("an unauthenticated request renamed a voice")
	}
}

// The card can play what the voice was cloned from. The clip is session-gated
// and marked private; RunPod still gets the bytes base64-inline, never a link.
func TestVoiceReferenceStreamsStoredBytes(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	want := []byte("reference-audio-bytes-1234567890")
	id := uploadClone(t, srv, cookie, "marrow.wav", want)

	rec := get(t, srv, cookie, "/voices/"+strconv.FormatInt(id, 10)+"/reference")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "audio/") {
		t.Errorf("Content-Type = %q, want an audio/* type", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control = %q, want it marked private", cc)
	}
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("streamed %q, want the stored bytes %q", rec.Body.Bytes(), want)
	}
}

func TestVoiceReferenceMissingForStockVoice(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	stockID := firstVoiceID(t, srv, cookie)

	rec := get(t, srv, cookie, "/voices/"+strconv.FormatInt(stockID, 10)+"/reference")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (a stock voice has no reference)", rec.Code)
	}
}

func TestVoiceReferenceUnauthenticatedRejected(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	id := uploadClone(t, srv, cookie, "clip.wav", []byte("reference-bytes"))

	rec := get(t, srv, nil, "/voices/"+strconv.FormatInt(id, 10)+"/reference")

	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 401 or 302", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("reference-bytes")) {
		t.Error("an unauthenticated request read a reference clip")
	}
}

// enqueued queues one job through the real route and returns it.
func enqueued(t *testing.T, srv *Server, cookie *http.Cookie, text string) jobs.Job {
	t.Helper()

	rec := postJob(t, srv, cookie, url.Values{
		"text":     {text},
		"voice_id": {strconv.FormatInt(firstVoiceID(t, srv, cookie), 10)},
	}, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("enqueue status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	items, err := srv.jobs.ListForUser(context.Background(), 1, 10)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("no job was queued")
	}
	return items[0]
}

// Every take records what rendered it, so a library of WAVs stays attributable
// once this rack runs more than one model.
func TestCreateJobRecordsTheModel(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)

	job := enqueued(t, srv, cookie, "Which model made this?")

	if job.Model == "" {
		t.Fatal("queued job records no model")
	}
	if job.Model != jobs.DefaultModel {
		t.Errorf("Model = %q, want %q", job.Model, jobs.DefaultModel)
	}
}

// The queue's poll carries the selected take, and the server marks that row —
// which is what keeps the highlight through a swap every two seconds.
func TestQueueFragmentMarksTheSelectedTake(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	job := enqueued(t, srv, cookie, "Select me.")

	rec := get(t, srv, cookie, "/jobs?take="+strconv.FormatInt(job.ID, 10))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	row := rec.Body.String()
	if !strings.Contains(row, `aria-selected="true"`) {
		t.Errorf("no row is marked selected: %s", row)
	}
	if strings.Contains(row, "<audio") {
		t.Error("the polled fragment contains an <audio> element; playback would restart on the tick")
	}
}

// Selecting a row loads that take into the player through its own route.
func TestJobPlayerFragment(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	job := enqueued(t, srv, cookie, "Play me.")

	rec := get(t, srv, cookie, "/jobs/"+strconv.FormatInt(job.ID, 10)+"/player")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Take "+strconv.FormatInt(job.ID, 10)) {
		t.Errorf("player fragment does not name the take: %s", rec.Body.String())
	}
}

func TestJobPlayerUnknownTake(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)

	if rec := get(t, srv, cookie, "/jobs/4242/player"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// The studio renders the player and the queue as separate regions, so the poll
// has something to swap that is not the player.
func TestStudioRendersPlayerOutsideTheQueue(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)

	rec := get(t, srv, cookie, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	queueAt := strings.Index(body, `id="queue"`)
	playerAt := strings.Index(body, `id="playback-body"`)
	if queueAt < 0 || playerAt < 0 {
		t.Fatalf("studio is missing the queue (%d) or the player (%d)", queueAt, playerAt)
	}
	if playerAt < queueAt {
		t.Fatal("the player renders inside the polled queue fragment")
	}
}

// The upload route still names a clone after its file — rename is the fix for
// that, not a replacement for it.
func TestUploadStillDerivesAName(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	id := uploadClone(t, srv, cookie, "narrator.wav", []byte("bytes"))

	if got := voiceName(t, srv, id); got != "narrator" {
		t.Errorf("derived name = %q, want narrator", got)
	}
	if v, err := srv.voices.Get(context.Background(), id); err != nil {
		t.Fatalf("voices.Get: %v", err)
	} else if v.Kind != voices.KindCloned {
		t.Errorf("Kind = %q, want cloned", v.Kind)
	}
}
