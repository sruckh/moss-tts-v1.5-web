package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/config"
	"github.com/sruckh/timbre/internal/voices"
)

// newUploadRequest builds a multipart/form-data POST carrying a single file
// part, the way the voice-library dropzone does.
func newUploadRequest(t *testing.T, field, filename string, data []byte) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/voices/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func voiceCount(t *testing.T, srv *Server) int {
	t.Helper()
	var count int
	if err := srv.db.QueryRow("SELECT COUNT(*) FROM voices").Scan(&count); err != nil {
		t.Fatalf("count voices: %v", err)
	}
	return count
}

// clonedID returns the most recently inserted cloned voice's id, failing if
// none exists.
func clonedID(t *testing.T, srv *Server) int64 {
	t.Helper()
	var id int64
	if err := srv.db.QueryRow(
		`SELECT id FROM voices WHERE kind = 'cloned' ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("find cloned voice: %v", err)
	}
	return id
}

func TestVoiceLibraryRenders(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/voices", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Voice library",
		"MOSS-TTS v1.5",      // the one model this rack runs
		"OpenMOSS Community", // its license badge, informational
	} {
		if !strings.Contains(body, want) {
			t.Errorf("library view missing %q", want)
		}
	}
}

func TestVoiceLibraryJSON(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/voices", nil)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var vs []voices.Voice
	if err := json.Unmarshal(rec.Body.Bytes(), &vs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("len = %d, want the single stock voice (the MOSS default)", len(vs))
	}
	if vs[0].Name != "Moss" || vs[0].Model != "MOSS-TTS v1.5" {
		t.Errorf("stock voice = %q (%q), want Moss (MOSS-TTS v1.5)", vs[0].Name, vs[0].Model)
	}
}

func TestVoiceUploadAuthenticated(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)

	before := voiceCount(t, srv)
	want := []byte("reference-audio-bytes-1234567890")

	req := newUploadRequest(t, "reference", "narrator.wav", want)
	req.AddCookie(cookie)
	req.Header.Set("HX-Request", "true") // mimic the dropzone's HTMX submit
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if after := voiceCount(t, srv); after != before+1 {
		t.Fatalf("voice count %d -> %d, want +1 (a new cloned row)", before, after)
	}

	// (2) The stored bytes read back exactly and base64 round-trip — readiness
	// for Goals 4–5's inline delivery.
	got, err := srv.voices.ReferenceBytes(context.Background(), clonedID(t, srv))
	if err != nil {
		t.Fatalf("ReferenceBytes: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stored reference bytes mismatch: got %q, want %q", got, want)
	}

	// (4) The refreshed grid fragment contains the uploaded clone next to stock.
	body := rec.Body.String()
	if !strings.Contains(body, "narrator") {
		t.Errorf("grid missing the uploaded clone name; body=%s", body)
	}
	if !strings.Contains(body, "MOSS-TTS v1.5") {
		t.Errorf("grid dropped the stock voice; body=%s", body)
	}
}

func TestVoiceGridFragmentPollsUntilTranscriptIsReady(t *testing.T) {
	srv := newTestServer(t)
	cookie := signInAs(t, srv, "grid_transcription", auth.StatusApproved)
	voiceID := uploadClone(t, srv, cookie, "pending.wav", []byte("pending-reference"))

	pending := do(t, srv, http.MethodGet, "/voices/grid", cookie)
	if pending.Code != http.StatusOK {
		t.Fatalf("pending grid status = %d, want 200", pending.Code)
	}
	if !strings.Contains(pending.Body.String(), `hx-get="/voices/grid"`) ||
		!strings.Contains(pending.Body.String(), "Transcribing...") {
		t.Fatalf("pending grid is not polling: %s", pending.Body.String())
	}

	if err := srv.voices.SetReferenceTranscript(context.Background(), voiceID, "Reference words."); err != nil {
		t.Fatalf("SetReferenceTranscript: %v", err)
	}
	ready := do(t, srv, http.MethodGet, "/voices/grid", cookie)
	if ready.Code != http.StatusOK {
		t.Fatalf("ready grid status = %d, want 200", ready.Code)
	}
	if strings.Contains(ready.Body.String(), `hx-get="/voices/grid"`) {
		t.Fatal("ready grid continues polling")
	}
	if !strings.Contains(ready.Body.String(), ">Ready</span>") {
		t.Fatalf("ready grid did not repaint badge: %s", ready.Body.String())
	}

	unauthenticated := do(t, srv, http.MethodGet, "/voices/grid", nil)
	if unauthenticated.Code != http.StatusFound {
		t.Fatalf("unauthenticated grid status = %d, want 302", unauthenticated.Code)
	}
}

func TestVoiceOwnerDeleteRemovesCardRecordAndReference(t *testing.T) {
	var audioDir string
	srv := newTestServerWithConfig(t, func(cfg *config.Config) { audioDir = cfg.AudioDir })
	cookie := signInAs(t, srv, "voice_delete_owner", auth.StatusApproved)
	voiceID := uploadClone(t, srv, cookie, "delete-me.wav", []byte("reference-to-delete"))
	voice, err := srv.voices.Get(context.Background(), voiceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	referencePath := filepath.Join(audioDir, voice.ReferencePath)

	rec := do(t, srv, http.MethodDelete, "/voices/"+strconv.FormatInt(voiceID, 10), cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "delete-me") {
		t.Fatalf("deleted card remains in grid: %s", rec.Body.String())
	}
	if _, err := srv.voices.Get(context.Background(), voiceID); !errors.Is(err, voices.ErrNotFound) {
		t.Fatalf("Get after delete error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(referencePath); !os.IsNotExist(err) {
		t.Fatalf("reference file still exists or stat failed: %v", err)
	}
	var assignments int
	if err := srv.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM voice_assignments WHERE voice_id = ?`, voiceID).Scan(&assignments); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	if assignments != 0 {
		t.Fatalf("assignments = %d, want 0", assignments)
	}
}

func TestVoiceDeleteRejectsAssignedNonCreatorAndStock(t *testing.T) {
	srv := newTestServer(t)
	ownerCookie := signInAs(t, srv, "voice_creator", auth.StatusApproved)
	assigneeCookie := signInAs(t, srv, "voice_assignee", auth.StatusApproved)
	assigneeID := userIDByName(t, srv, "voice_assignee")
	voiceID := uploadClone(t, srv, ownerCookie, "creator-only.wav", []byte("private-reference"))
	if err := srv.voices.Assign(context.Background(), voiceID, assigneeID); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	rec := do(t, srv, http.MethodDelete, "/voices/"+strconv.FormatInt(voiceID, 10), assigneeCookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("assignee delete status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if _, err := srv.voices.Get(context.Background(), voiceID); err != nil {
		t.Fatalf("voice disappeared after forbidden delete: %v", err)
	}

	stockID := firstVoiceID(t, srv, ownerCookie)
	rec = do(t, srv, http.MethodDelete, "/voices/"+strconv.FormatInt(stockID, 10), ownerCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stock delete status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestAdminCanDeleteAnotherUsersVoice(t *testing.T) {
	srv := newTestServer(t)
	adminCookie := login(t, srv)
	ownerCookie := signInAs(t, srv, "admin_voice_target", auth.StatusApproved)
	voiceID := uploadClone(t, srv, ownerCookie, "admin-delete.wav", []byte("admin-reference"))

	rec := adminAction(t, srv, adminCookie, http.MethodDelete,
		"/admin/voices/"+strconv.FormatInt(voiceID, 10), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin delete status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if _, err := srv.voices.Get(context.Background(), voiceID); !errors.Is(err, voices.ErrNotFound) {
		t.Fatalf("Get after admin delete error = %v, want ErrNotFound", err)
	}
}

func TestVoiceUploadUnauthenticatedRejected(t *testing.T) {
	srv := newTestServer(t)
	before := voiceCount(t, srv)

	req := newUploadRequest(t, "reference", "clip.wav", []byte("x"))
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upload status = %d, want 401", rec.Code)
	}
	if after := voiceCount(t, srv); after != before {
		t.Errorf("a row was created by an unauthenticated upload: %d -> %d", before, after)
	}
}

func TestVoiceUploadRejectsBadType(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	before := voiceCount(t, srv)

	req := newUploadRequest(t, "reference", "notes.txt", []byte("not audio"))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
	if after := voiceCount(t, srv); after != before {
		t.Errorf("a row was created for a rejected file type: %d -> %d", before, after)
	}
}

func TestVoiceUploadRejectsOversize(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	before := voiceCount(t, srv)

	big := bytes.Repeat([]byte{0xAA}, 10*1024*1024+1024) // just over the 10 MB cap
	req := newUploadRequest(t, "reference", "big.wav", big)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if after := voiceCount(t, srv); after != before {
		t.Errorf("a row was created for an oversize upload: %d -> %d", before, after)
	}
}

func TestVoiceLibraryFiltersOtherUsersClones(t *testing.T) {
	srv := newTestServer(t)
	cookieA := signInAs(t, srv, "user_a", "approved")
	cookieB := signInAs(t, srv, "user_b", "approved")

	// User A uploads a clone
	uploadReq := newUploadRequest(t, "reference", "clone_a.wav", []byte("wavdata"))
	uploadReq.AddCookie(cookieA)
	uploadReq.Header.Set("HX-Request", "true")
	recUpload := httptest.NewRecorder()
	srv.ServeHTTP(recUpload, uploadReq)
	if recUpload.Code != http.StatusOK {
		t.Fatalf("upload by user_a = %d, want 200", recUpload.Code)
	}

	// User A requests voice library -> sees stock + clone_a
	reqA := httptest.NewRequest(http.MethodGet, "/voices", nil)
	reqA.Header.Set("Accept", "application/json")
	reqA.AddCookie(cookieA)
	recA := httptest.NewRecorder()
	srv.ServeHTTP(recA, reqA)
	var listA []voices.Voice
	_ = json.Unmarshal(recA.Body.Bytes(), &listA)
	if len(listA) != 2 {
		t.Fatalf("User A voice count = %d, want 2", len(listA))
	}

	// User B requests voice library -> sees stock only (does not see clone_a)
	reqB := httptest.NewRequest(http.MethodGet, "/voices", nil)
	reqB.Header.Set("Accept", "application/json")
	reqB.AddCookie(cookieB)
	recB := httptest.NewRecorder()
	srv.ServeHTTP(recB, reqB)
	var listB []voices.Voice
	_ = json.Unmarshal(recB.Body.Bytes(), &listB)
	if len(listB) != 1 {
		t.Fatalf("User B voice count = %d, want 1 (stock only)", len(listB))
	}
}

func TestSubmitJobRejectsNonOwnerVoice(t *testing.T) {
	srv := newTestServer(t)
	cookieA := signInAs(t, srv, "user_a", "approved")
	cookieB := signInAs(t, srv, "user_b", "approved")

	// User A uploads a clone
	uploadReq := newUploadRequest(t, "reference", "clone_a.wav", []byte("wavdata"))
	uploadReq.AddCookie(cookieA)
	recUpload := httptest.NewRecorder()
	srv.ServeHTTP(recUpload, uploadReq)
	if recUpload.Code != http.StatusOK {
		t.Fatalf("upload by user_a = %d, want 200", recUpload.Code)
	}
	cloneIDA := clonedID(t, srv)

	// User B attempts to create job with User A's cloned voice -> 403 Forbidden
	form := strings.NewReader("voice_id=" + strconv.FormatInt(cloneIDA, 10) + "&text=Hello+world")
	reqJob := httptest.NewRequest(http.MethodPost, "/jobs", form)
	reqJob.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqJob.AddCookie(cookieB)
	recJob := httptest.NewRecorder()
	srv.ServeHTTP(recJob, reqJob)

	if recJob.Code != http.StatusForbidden {
		t.Fatalf("User B submitting User A's voice status = %d, want 403", recJob.Code)
	}
}

func TestSubmitJobAcceptsAssignedVoice(t *testing.T) {
	srv := newTestServer(t)
	cookieA := signInAs(t, srv, "user_a", "approved")
	cookieB := signInAs(t, srv, "user_b", "approved")

	uploadReq := newUploadRequest(t, "reference", "clone_a.wav", []byte("wavdata"))
	uploadReq.AddCookie(cookieA)
	recUpload := httptest.NewRecorder()
	srv.ServeHTTP(recUpload, uploadReq)
	if recUpload.Code != http.StatusOK {
		t.Fatalf("upload by user_a = %d, want 200", recUpload.Code)
	}
	voiceID := clonedID(t, srv)
	userB := userIDByName(t, srv, "user_b")
	if err := srv.voices.Assign(context.Background(), voiceID, userB); err != nil {
		t.Fatalf("Assign voice to user_b: %v", err)
	}

	form := strings.NewReader("voice_id=" + strconv.FormatInt(voiceID, 10) + "&text=Hello+world")
	reqJob := httptest.NewRequest(http.MethodPost, "/jobs", form)
	reqJob.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqJob.AddCookie(cookieB)
	recJob := httptest.NewRecorder()
	srv.ServeHTTP(recJob, reqJob)
	if recJob.Code != http.StatusOK {
		t.Fatalf("assigned user submit status = %d, want 200 (body %q)", recJob.Code, recJob.Body.String())
	}
}
