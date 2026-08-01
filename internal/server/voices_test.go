package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	items, err := srv.voices.List(context.Background())
	if err != nil {
		t.Fatalf("voices.List: %v", err)
	}
	return len(items)
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
		"Chatterbox", "Qwen3-TTS", "Higgs Audio v2", // stock models
		"Commercial-safe", // license badge
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
	if len(vs) != 3 {
		t.Fatalf("len = %d, want 3 stock voices", len(vs))
	}
	names := map[string]bool{}
	for _, v := range vs {
		names[v.Name] = true
	}
	for _, want := range []string{"Ash", "Vellum", "Slate"} {
		if !names[want] {
			t.Errorf("missing stock voice %q in JSON", want)
		}
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
	if !strings.Contains(body, "Chatterbox") {
		t.Errorf("grid dropped the stock voices; body=%s", body)
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
