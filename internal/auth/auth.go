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
	"net/mail"
	"strings"
	"unicode"

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

// Session Values keys. role and status are snapshots taken at login, so a gate
// can answer "may this request proceed" without a query per request.
const (
	userIDKey = "user_id"
	roleKey   = "role"
	statusKey = "status"
)

// The values users.role and users.status may hold. The database CHECK
// constraints enforce the same two sets; these are the Go-side names for them,
// so a gate never spells one as a bare string.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	StatusApproved = "approved"
	StatusPending  = "pending"
	StatusDisabled = "disabled"
)

// ErrInvalidCredentials is returned by Login for a bad username or password.
// The two are deliberately indistinguishable to the caller.
var ErrInvalidCredentials = errors.New("invalid username or password")

// Registration rejections. Each is a fixed sentence safe to show an anonymous
// applicant — none of them echoes what was submitted.
var (
	ErrUsernameTaken   = errors.New("that username is taken")
	ErrInvalidUsername = errors.New("username must be 3-64 characters and contain no spaces")
	ErrWeakPassword    = errors.New("password must be 12-72 characters")
	ErrInvalidEmail    = errors.New("that email address is not valid")
)

const (
	minUsernameLen = 3
	maxUsernameLen = 64
	minPasswordLen = 12
	// bcrypt hashes at most 72 bytes and silently drops the rest, so a longer
	// password would be weaker than the person typing it believes.
	maxPasswordLen = 72
)

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
	// Explicitly admin/approved. The column defaults describe an applicant, and
	// on a fresh install Bootstrap runs *after* Migrate — so the migration's
	// re-promotion of the lowest-id user has already happened and would not
	// reach this row until the next restart, leaving the first admin locked out
	// of its own app in the meantime.
	if _, err := m.db.ExecContext(ctx,
		"INSERT INTO users (username, password_hash, role, status) VALUES (?, ?, ?, ?)",
		username, hash, RoleAdmin, StatusApproved); err != nil {
		return fmt.Errorf("bootstrap: insert admin: %w", err)
	}
	log.Info("admin user created", "username", username)
	return nil
}

// Register creates a self-service account and returns its id. The row is always
// role 'user' and status 'pending', written explicitly rather than left to the
// column defaults so that changing a default can never silently hand out
// access. Registering is a request for access, not a grant of it: no session is
// issued here and the caller must not sign the applicant in.
//
// Admins are never self-service — the first one comes from Bootstrap, later
// ones are promoted by an existing admin.
// Register creates a self-service account and returns its id. The row is always
// role 'user' and status 'pending', written explicitly rather than left to the
// column defaults so that changing a default can never silently hand out
// access. Registering is a request for access, not a grant of it: no session is
// issued here and the caller must not sign the applicant in.
//
// Admins are never self-service — the first one comes from Bootstrap, later
// ones are promoted by an existing admin.
func (m *Manager) Register(ctx context.Context, username, email, password string) (int64, error) {
	username, email, err := validateApplication(username, email, password)
	if err != nil {
		return 0, err
	}

	hash, err := HashPassword(password)
	if err != nil {
		return 0, fmt.Errorf("register: %w", err)
	}

	res, err := m.db.ExecContext(ctx,
		"INSERT INTO users (username, password_hash, email, role, status) VALUES (?, ?, ?, ?, ?)",
		username, hash, nullable(email), RoleUser, StatusPending)
	if err != nil {
		// users.username is UNIQUE, so the constraint is the race-free answer to
		// "is this name taken": a SELECT first and an INSERT second is not, two
		// applicants can pass the check between them. The driver reports it as a
		// message rather than a typed error, hence the string test.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, ErrUsernameTaken
		}
		return 0, fmt.Errorf("register: insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("register: new user id: %w", err)
	}
	return id, nil
}

// validateApplication normalises and checks what someone submits when asking
// for access, whether that becomes a user row (Register) or an access request
// (AccessRequests.Create). Both must agree, or a request could be accepted and
// then fail to become an account.
func validateApplication(username, email, password string) (string, string, error) {
	username = strings.TrimSpace(username)
	email = strings.TrimSpace(email)

	if len(username) < minUsernameLen || len(username) > maxUsernameLen ||
		strings.ContainsFunc(username, unicode.IsSpace) {
		return "", "", ErrInvalidUsername
	}
	if len(password) < minPasswordLen || len(password) > maxPasswordLen {
		return "", "", ErrWeakPassword
	}
	if email != "" {
		if _, err := mail.ParseAddress(email); err != nil {
			return "", "", ErrInvalidEmail
		}
	}
	return username, email, nil
}

// nullable keeps an absent value NULL rather than "". users.email and
// access_requests.email are both nullable, and "" would record a value where
// there is none.
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// nullableID is nullable for a foreign key: an undecided request has no
// decided_by, and 0 is not a user id.
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// Login verifies credentials and, on success, issues the session cookie.
// Login verifies credentials and, on success, issues the session cookie
// carrying the user's id, role and status.
//
// It deliberately admits a 'pending' or 'disabled' user: signing in and being
// allowed into the studio are separate questions, and answering the second one
// here would leave the holding screen with no session to identify its visitor.
// The approval gate reads Status off the session instead.
func (m *Manager) Login(w http.ResponseWriter, r *http.Request, username, password string) error {
	var (
		id     int64
		hash   string
		role   string
		status string
	)
	err := m.db.QueryRowContext(r.Context(),
		"SELECT id, password_hash, role, status FROM users WHERE username = ?", username).
		Scan(&id, &hash, &role, &status)
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
	session.Values[roleKey] = role
	session.Values[statusKey] = status
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
// UserID returns the authenticated user's id, or false when the request has
// no valid session.
func (m *Manager) UserID(r *http.Request) (int64, bool) {
	who, ok := m.Current(r)
	return who.UserID, ok
}

// Role returns the session's role, or false when the request has no valid
// session.
func (m *Manager) Role(r *http.Request) (string, bool) {
	who, ok := m.Current(r)
	return who.Role, ok
}

// Status returns the session's approval status, or false when the request has
// no valid session. A gate wanting both fields should call Current once rather
// than this and Role in turn.
func (m *Manager) Status(r *http.Request) (string, bool) {
	who, ok := m.Current(r)
	return who.Status, ok
}

// Identity is who the session says the requester is.
type Identity struct {
	UserID int64
	Role   string
	Status string
}

// Current reads the whole identity out of the session in one load, which is
// what lets a gate check role and status without paying for the session twice.
func (m *Manager) Current(r *http.Request) (Identity, bool) {
	session, err := m.store.Get(r, SessionName)
	if err != nil || session.IsNew {
		return Identity{}, false
	}
	id, ok := session.Values[userIDKey].(int64)
	if !ok {
		return Identity{}, false
	}
	who := Identity{UserID: id}
	who.Role, _ = session.Values[roleKey].(string)
	who.Status, _ = session.Values[statusKey].(string)
	if who.Role == "" || who.Status == "" {
		// A session minted before login started recording these. Reading them
		// back keeps an already-signed-in user working across the upgrade rather
		// than bouncing them off a gate they would pass. Self-limiting: every
		// such session ages out within sessionMaxAge, after which this branch is
		// dead and can be deleted.
		if err := m.db.QueryRowContext(r.Context(),
			"SELECT role, status FROM users WHERE id = ?", id).Scan(&who.Role, &who.Status); err != nil {
			return Identity{}, false
		}
	}
	return who, true
}

// ErrNoSuchUser is returned when a session names a user that no longer exists —
// deleted while they were signed in.
var ErrNoSuchUser = errors.New("no such user")

// LiveIdentity reads role and status straight from the users table, ignoring
// the snapshot Login left in the session.
//
// The approval gate uses this rather than Current so that disabling or demoting
// someone takes effect on their very next request, instead of whenever their
// week-old session happens to expire. It costs one indexed lookup by primary
// key per gated request, which is the price of not having to hunt down and
// invalidate a user's sessions from every place that can change their status.
func (m *Manager) LiveIdentity(ctx context.Context, userID int64) (Identity, error) {
	who := Identity{UserID: userID}
	err := m.db.QueryRowContext(ctx,
		"SELECT role, status FROM users WHERE id = ?", userID).Scan(&who.Role, &who.Status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Identity{}, ErrNoSuchUser
	case err != nil:
		return Identity{}, fmt.Errorf("live identity: %w", err)
	}
	return who, nil
}

// exemptExact lists the only whole paths reachable without a session.
var exemptExact = map[string]bool{
	"/login":   true,
	"/healthz": true,
	// Applying for an account cannot require an account. The handler creates a
	// 'pending' user and issues no session, so exempting it grants nothing.
	"/register": true,
	// The public application flow, for the same reason: /apply writes an
	// access_requests row and /apply/status reads one word back out of it.
	// Neither touches a session, so neither can hand one out.
	"/apply":        true,
	"/apply/status": true,
}

// exemptPrefixes lists the only path prefixes reachable without a session:
// static assets (the login page needs its stylesheet). Reference audio is
// delivered to RunPod base64-inline and is never served over HTTP, so there is
// no public reference route to exempt.
var exemptPrefixes = []string{"/static/"}

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
