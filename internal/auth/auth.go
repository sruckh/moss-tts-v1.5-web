// Package auth owns password hashing, the admin bootstrap, sessions and the
// middleware that gates every non-exempt route behind a login.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gorilla/sessions"
	"golang.org/x/crypto/bcrypt"
)

// SessionName is the cookie the browser carries. Its value is only the
// signed session ID — the session data itself lives server-side in SQLite,
// so logging out genuinely invalidates the cookie.
const SessionName = "timbre_session"

// sessionMaxAge is both the cookie's MaxAge and the server-side expiry,
// in seconds: one week.
const sessionMaxAge = 7 * 24 * 60 * 60

// userIDKey is the session Values key holding the authenticated user's id.
const userIDKey = "user_id"

// ErrInvalidCredentials is returned by Login for a bad username or password.
// The two are deliberately indistinguishable to the caller.
var ErrInvalidCredentials = errors.New("invalid username or password")

// dummyHash keeps Login's timing uniform when the username does not exist:
// we still pay one bcrypt compare, so the response time doesn't reveal which
// half failed. Cost is paid once at package init.
var dummyHash = mustHash("timing-equalizer")

func mustHash(pw string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return string(h)
}

// HashPassword bcrypts a plaintext password at the default cost.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// CheckPassword reports whether password matches the bcrypt hash.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// Manager ties the user table to the session store and the middleware.
type Manager struct {
	db    *sql.DB
	store *SQLiteStore
}

// NewManager creates the sessions table if needed and returns the manager.
// secret keys the cookie-signing HMAC; when empty, a random key is generated
// and ephemeral is true — the caller should warn that sessions will not
// survive a restart. secureCookies sets the cookie Secure flag (behind HTTPS).
func NewManager(db *sql.DB, secret []byte, secureCookies bool) (m *Manager, ephemeral bool, err error) {
	if _, err := db.Exec(sessionSchema); err != nil {
		return nil, false, fmt.Errorf("auth schema: %w", err)
	}

	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, false, fmt.Errorf("generate ephemeral session key: %w", err)
		}
		ephemeral = true
	}
	// Any length of configured secret becomes a fixed 32-byte HMAC key.
	sum := sha256.Sum256(secret)

	store := NewSQLiteStore(db, &sessions.Options{
		Path:     "/",
		MaxAge:   sessionMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secureCookies,
	}, sum[:])
	// Best-effort hygiene; a failure here is not worth refusing to start.
	_ = store.PurgeExpired(context.Background())

	return &Manager{db: db, store: store}, ephemeral, nil
}

// Bootstrap seeds the first admin user when the users table is empty. It runs
// on every startup and is a no-op once any user exists. With an empty table
// and no credentials configured it warns — the app runs, but nobody can sign
// in. The password is never logged.
func (m *Manager) Bootstrap(ctx context.Context, log *slog.Logger, username, password string) error {
	var count int
	if err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return fmt.Errorf("bootstrap: count users: %w", err)
	}
	if count > 0 {
		return nil
	}
	if username == "" || password == "" {
		log.Warn("no users exist and ADMIN_USERNAME/ADMIN_PASSWORD are unset; " +
			"no one can sign in until a user is created")
		return nil
	}

	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx,
		"INSERT INTO users (username, password_hash) VALUES (?, ?)", username, hash); err != nil {
		return fmt.Errorf("bootstrap: insert admin: %w", err)
	}
	log.Info("admin user created", "username", username)
	return nil
}

// Login verifies credentials and, on success, issues the session cookie.
func (m *Manager) Login(w http.ResponseWriter, r *http.Request, username, password string) error {
	var (
		id   int64
		hash string
	)
	err := m.db.QueryRowContext(r.Context(),
		"SELECT id, password_hash FROM users WHERE username = ?", username).Scan(&id, &hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Uniform timing: compare against a real bcrypt hash anyway.
		CheckPassword(dummyHash, password)
		return ErrInvalidCredentials
	case err != nil:
		return fmt.Errorf("login: lookup user: %w", err)
	}
	if !CheckPassword(hash, password) {
		return ErrInvalidCredentials
	}

	session, err := m.store.Get(r, SessionName)
	if err != nil {
		return fmt.Errorf("login: load session: %w", err)
	}
	session.Values[userIDKey] = id
	if err := session.Save(r, w); err != nil {
		return fmt.Errorf("login: save session: %w", err)
	}
	return nil
}

// Logout destroys the server-side session and expires the cookie.
func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) error {
	session, err := m.store.Get(r, SessionName)
	if err != nil {
		return fmt.Errorf("logout: load session: %w", err)
	}
	session.Options.MaxAge = -1
	if err := session.Save(r, w); err != nil {
		return fmt.Errorf("logout: save session: %w", err)
	}
	return nil
}

// UserID returns the authenticated user's id, or false when the request has
// no valid session.
func (m *Manager) UserID(r *http.Request) (int64, bool) {
	session, err := m.store.Get(r, SessionName)
	if err != nil || session.IsNew {
		return 0, false
	}
	id, ok := session.Values[userIDKey].(int64)
	return id, ok
}

// exemptExact lists the only whole paths reachable without a session.
var exemptExact = map[string]bool{
	"/login":   true,
	"/healthz": true,
}

// exemptPrefixes lists the only path prefixes reachable without a session:
// static assets (the login page needs its stylesheet) and the public,
// token-gated reference-audio route RunPod's workers fetch (Goal 3).
var exemptPrefixes = []string{"/static/", "/refs/"}

// Exempt reports whether path is reachable without a session. This is the one
// place the public surface is defined — anything not listed here requires
// login.
func Exempt(path string) bool {
	if exemptExact[path] {
		return true
	}
	for _, prefix := range exemptPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// Middleware gates every non-exempt route behind a valid session. HTMX and
// JSON callers get a 401 (a 302 to an HTML page is useless to them); browser
// navigation gets redirected to the login page.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Exempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := m.UserID(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" ||
			strings.Contains(r.Header.Get("Accept"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"authentication required"}`)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}
