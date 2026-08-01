package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sruckh/timbre/internal/config"
	"github.com/sruckh/timbre/internal/db"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()

	handle, err := db.Open(filepath.Join(t.TempDir(), "timbre.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if err := db.Migrate(context.Background(), handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	return New(config.Config{RunPodEndpoint: "https://api.runpod.ai/v2/test-endpoint"}, handle)
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if !body["ok"] {
		t.Errorf("body = %v, want {\"ok\":true}", body)
	}
}

func TestStylesheetIsServed(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (is app.css generated?)", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("app.css is empty")
	}
}

func TestShellRenders(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); len(body) == 0 {
		t.Error("empty shell response")
	}
}
