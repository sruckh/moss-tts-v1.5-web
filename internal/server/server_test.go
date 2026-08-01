package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/config"
	"github.com/sruckh/timbre/internal/db"
	"github.com/sruckh/timbre/internal/voices"
)

const testPassword = "correct horse battery staple"

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

	mgr, _, err := auth.NewManager(handle, []byte("test-secret"), false)
	if err != nil {
		t.Fatalf("auth.NewManager: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := mgr.Bootstrap(context.Background(), log, "admin", testPassword); err != nil {
		t.Fatalf("auth.Bootstrap: %v", err)
	}

	cfg := config.Config{
		RunPodEndpoint: "https://api.runpod.ai/v2/test-endpoint",
		AudioDir:       t.TempDir(),
	}
	voiceStore := voices.NewStore(handle, cfg.AudioDir)
	if err := voiceStore.SeedStock(context.Background()); err != nil {
		t.Fatalf("voices.SeedStock: %v", err)
	}

	return New(cfg, handle, mgr, voiceStore)
}

// login posts valid credentials and returns the issued session cookie.
func login(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()

	form := url.Values{"username": {"admin"}, "password": {testPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionName {
			return c
		}
	}
	t.Fatal("login set no session cookie")
	return nil
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

func TestLoginPageIsPublic(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `action="/login"`) {
		t.Error("login page does not contain the login form")
	}
}

func TestShellRequiresAuth(t *testing.T) {
	srv := newTestServer(t)

	// No cookie → redirect to /login.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("unauthenticated status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}

	// Wrong credentials → 401, no session cookie.
	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad-login status = %d, want 401", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionName {
			t.Error("failed login set a session cookie")
		}
	}

	// Valid session → 200.
	cookie := login(t, srv)
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("empty shell response")
	}

	// Logout → the cookie stops working.
	req = httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("post-logout status = %d, want 302", rec.Code)
	}
}
