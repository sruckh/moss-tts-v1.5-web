package auth_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/db"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newManager opens a fresh temp-file database, migrates the app schema and
// returns a manager with a deterministic (non-ephemeral) signing key.
func newManager(t *testing.T) (*auth.Manager, func()) {
	t.Helper()

	handle, err := db.Open(filepath.Join(t.TempDir(), "timbre.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(context.Background(), handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	mgr, ephemeral, err := auth.NewManager(handle, []byte("test-secret"), false)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if ephemeral {
		t.Fatal("NewManager: ephemeral = true with a configured secret")
	}
	return mgr, func() { handle.Close() }
}

func bootstrapAdmin(t *testing.T, mgr *auth.Manager) {
	t.Helper()
	if err := mgr.Bootstrap(context.Background(), testLogger(), "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
}

// loginForm performs POST /login against handler and returns the recorder.
func loginForm(t *testing.T, handler http.HandlerFunc, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// sessionCookie extracts the session cookie from a response, or fails.
func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionName {
			return c
		}
	}
	t.Fatalf("no %q cookie in response", auth.SessionName)
	return nil
}

func TestHashAndCheckPassword(t *testing.T) {
	hash, err := auth.HashPassword("hunter2")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "hunter2" {
		t.Error("hash equals plaintext")
	}
	if !auth.CheckPassword(hash, "hunter2") {
		t.Error("CheckPassword: correct password rejected")
	}
	if auth.CheckPassword(hash, "hunter3") {
		t.Error("CheckPassword: wrong password accepted")
	}
}

func TestBootstrapCreatesAdminOnce(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()

	bootstrapAdmin(t, mgr)
	// Second call with different creds is a no-op — the first user wins.
	if err := mgr.Bootstrap(context.Background(), testLogger(), "other", "otherpw"); err != nil {
		t.Fatalf("Bootstrap (second): %v", err)
	}

	login := func(w http.ResponseWriter, r *http.Request) {
		_ = mgr.Login(w, r, r.PostFormValue("username"), r.PostFormValue("password"))
	}
	rec := loginForm(t, login, "other", "otherpw")
	if len(rec.Result().Cookies()) != 0 {
		t.Error("second bootstrap overwrote the admin: 'other' logged in")
	}
	rec = loginForm(t, login, "admin", "correct horse battery staple")
	sessionCookie(t, rec)
}

func TestBootstrapWithoutCredsIsNoop(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()

	if err := mgr.Bootstrap(context.Background(), testLogger(), "", ""); err != nil {
		t.Fatalf("Bootstrap with no creds: %v", err)
	}
	// No user exists, so no login can succeed.
	rec := loginForm(t, func(w http.ResponseWriter, r *http.Request) {
		_ = mgr.Login(w, r, r.PostFormValue("username"), r.PostFormValue("password"))
	}, "admin", "admin")
	if len(rec.Result().Cookies()) != 0 {
		t.Error("login succeeded with an empty users table")
	}
}

func TestLoginWrongCredentials(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()
	bootstrapAdmin(t, mgr)

	for _, creds := range [][2]string{
		{"admin", "wrong password"},
		{"nosuchuser", "correct horse battery staple"},
	} {
		err := mgr.Login(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/login", nil),
			creds[0], creds[1])
		if err != auth.ErrInvalidCredentials {
			t.Errorf("Login(%q, %q) = %v, want ErrInvalidCredentials", creds[0], creds[1], err)
		}
	}
}

func TestLoginSuccessSetsSignedCookie(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()
	bootstrapAdmin(t, mgr)

	rec := loginForm(t, func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Login(w, r, r.PostFormValue("username"), r.PostFormValue("password")); err != nil {
			t.Errorf("Login: %v", err)
		}
	}, "admin", "correct horse battery staple")

	c := sessionCookie(t, rec)
	if !c.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Value == "" {
		t.Error("empty session cookie value")
	}
}

func TestExemptList(t *testing.T) {
	for path, want := range map[string]bool{
		"/login":          true,
		"/healthz":        true,
		"/static/app.css": true,
		"/refs/abc.wav":   true, // Goal 3's public reference-audio route
		"/":               false,
		"/logout":         false,
		"/jobs":           false,
	} {
		if got := auth.Exempt(path); got != want {
			t.Errorf("Exempt(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestMiddleware(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()
	bootstrapAdmin(t, mgr)

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	protected := mgr.Middleware(ok)

	t.Run("no cookie redirects browser to login", func(t *testing.T) {
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("Location = %q, want /login", loc)
		}
	})

	t.Run("no cookie answers HTMX with 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("no cookie answers JSON with 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("exempt paths pass through", func(t *testing.T) {
		for _, path := range []string{"/login", "/healthz", "/static/app.css", "/refs/x.wav"} {
			rec := httptest.NewRecorder()
			protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("%s: status = %d, want 200", path, rec.Code)
			}
		}
	})

	t.Run("forged cookie is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionName, Value: "forged"})
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
	})

	t.Run("valid session passes", func(t *testing.T) {
		rec := loginForm(t, func(w http.ResponseWriter, r *http.Request) {
			_ = mgr.Login(w, r, r.PostFormValue("username"), r.PostFormValue("password"))
		}, "admin", "correct horse battery staple")
		cookie := sessionCookie(t, rec)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(cookie)
		rec = httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}

func TestLogoutInvalidatesSession(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()
	bootstrapAdmin(t, mgr)

	// Log in and capture the cookie.
	rec := loginForm(t, func(w http.ResponseWriter, r *http.Request) {
		_ = mgr.Login(w, r, r.PostFormValue("username"), r.PostFormValue("password"))
	}, "admin", "correct horse battery staple")
	cookie := sessionCookie(t, rec)

	// Log out with it.
	logoutReq := httptest.NewRequest(http.MethodPost, "/logout", nil)
	logoutReq.AddCookie(cookie)
	logoutRec := httptest.NewRecorder()
	if err := mgr.Logout(logoutRec, logoutReq); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	// The response must expire the browser's cookie…
	var cleared *http.Cookie
	for _, c := range logoutRec.Result().Cookies() {
		if c.Name == auth.SessionName {
			cleared = c
		}
	}
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("logout did not expire the cookie: %+v", cleared)
	}

	// …and replaying the pre-logout cookie must no longer authenticate.
	protected := mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	replayReq := httptest.NewRequest(http.MethodGet, "/", nil)
	replayReq.AddCookie(cookie)
	replayRec := httptest.NewRecorder()
	protected.ServeHTTP(replayRec, replayReq)
	if replayRec.Code != http.StatusFound {
		t.Fatalf("replayed cookie after logout: status = %d, want 302", replayRec.Code)
	}
}
