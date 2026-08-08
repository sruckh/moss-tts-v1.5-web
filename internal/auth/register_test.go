package auth_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gorilla/sessions"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/db"
)

const applicantPassword = "a perfectly fine passphrase"

// newAuthDB is newManager plus the handle, for the few assertions that have to
// look at the users row itself rather than at what a session reports.
func newAuthDB(t *testing.T) (*auth.Manager, *sql.DB) {
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
	return mgr, handle
}

// loginIdentity signs the user in and reads the identity back out of the issued
// cookie — the only way to observe what Login actually put in the session.
func loginIdentity(t *testing.T, mgr *auth.Manager, username, password string) auth.Identity {
	t.Helper()

	rec := loginForm(t, func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Login(w, r, r.PostFormValue("username"), r.PostFormValue("password")); err != nil {
			t.Errorf("Login: %v", err)
		}
	}, username, password)

	next := httptest.NewRequest(http.MethodGet, "/", nil)
	next.AddCookie(sessionCookie(t, rec))
	who, ok := mgr.Current(next)
	if !ok {
		t.Fatal("the session Login issued is not readable")
	}
	return who
}

// The bootstrapped admin has to be usable on the first boot. Bootstrap runs
// after Migrate on a fresh install, so leaving this to the migration's
// re-promotion would strand the first admin as 'pending' until a restart.
func TestBootstrapAdminIsAdminAndApproved(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()
	bootstrapAdmin(t, mgr)

	who := loginIdentity(t, mgr, "admin", "correct horse battery staple")
	if who.Role != auth.RoleAdmin || who.Status != auth.StatusApproved {
		t.Errorf("bootstrapped admin = (%q, %q), want (%q, %q)",
			who.Role, who.Status, auth.RoleAdmin, auth.StatusApproved)
	}
}

// Signing in and being let into the studio are separate questions. A pending
// applicant must be able to log in — the holding screen stage 03 adds needs a
// session to know who is waiting — and the session must say 'pending' so that
// gate can turn them away.
func TestPendingUserLogsInAndSessionSaysPending(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()
	bootstrapAdmin(t, mgr)

	if _, err := mgr.Register(context.Background(), "applicant", "", applicantPassword); err != nil {
		t.Fatalf("Register: %v", err)
	}

	who := loginIdentity(t, mgr, "applicant", applicantPassword)
	if who.Role != auth.RoleUser || who.Status != auth.StatusPending {
		t.Errorf("applicant session = (%q, %q), want (%q, %q)",
			who.Role, who.Status, auth.RoleUser, auth.StatusPending)
	}
	if who.UserID == 0 {
		t.Error("the applicant's session carries no user id")
	}
}

// Registering stores a bcrypt hash, never the password, and an omitted email
// stays NULL rather than becoming an empty string.
func TestRegisterStoresHashedPasswordAndNullEmail(t *testing.T) {
	mgr, handle := newAuthDB(t)
	ctx := context.Background()

	id, err := mgr.Register(ctx, "applicant", "", applicantPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	var hash string
	var email sql.Null[string]
	var role, status string
	if err := handle.QueryRowContext(ctx,
		"SELECT password_hash, email, role, status FROM users WHERE id = ?", id).
		Scan(&hash, &email, &role, &status); err != nil {
		t.Fatalf("read the new user: %v", err)
	}
	if hash == applicantPassword {
		t.Fatal("the password was stored in plaintext")
	}
	if !auth.CheckPassword(hash, applicantPassword) {
		t.Error("the stored hash does not verify the password")
	}
	if email.Valid {
		t.Errorf("email = %q, want NULL when none was supplied", email.V)
	}
	if role != auth.RoleUser || status != auth.StatusPending {
		t.Errorf("new user = (%q, %q), want (%q, %q)", role, status, auth.RoleUser, auth.StatusPending)
	}
}

func TestRegisterKeepsASuppliedEmail(t *testing.T) {
	mgr, handle := newAuthDB(t)
	ctx := context.Background()

	// Deliberately padded: the handler passes the raw form value through.
	id, err := mgr.Register(ctx, "  applicant  ", "  applicant@example.com  ", applicantPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	var username string
	var email sql.Null[string]
	if err := handle.QueryRowContext(ctx,
		"SELECT username, email FROM users WHERE id = ?", id).Scan(&username, &email); err != nil {
		t.Fatalf("read the new user: %v", err)
	}
	if username != "applicant" {
		t.Errorf("username = %q, want the surrounding space trimmed", username)
	}
	if !email.Valid || email.V != "applicant@example.com" {
		t.Errorf("email = %+v, want applicant@example.com", email)
	}
}

func TestRegisterRejectsATakenUsername(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := mgr.Register(ctx, "applicant", "", applicantPassword); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	_, err := mgr.Register(ctx, "applicant", "", "a different passphrase")
	if !errors.Is(err, auth.ErrUsernameTaken) {
		t.Errorf("second Register error = %v, want ErrUsernameTaken", err)
	}
}

// The bootstrapped admin's name is taken like any other.
func TestRegisterCannotStealTheAdminUsername(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()
	bootstrapAdmin(t, mgr)

	_, err := mgr.Register(context.Background(), "admin", "", applicantPassword)
	if !errors.Is(err, auth.ErrUsernameTaken) {
		t.Errorf("Register error = %v, want ErrUsernameTaken", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()

	cases := []struct {
		name                     string
		username, email, passwrd string
		want                     error
	}{
		{"empty username", "", "", applicantPassword, auth.ErrInvalidUsername},
		{"username too short", "ab", "", applicantPassword, auth.ErrInvalidUsername},
		{"username all spaces", "     ", "", applicantPassword, auth.ErrInvalidUsername},
		{"username with a space", "two words", "", applicantPassword, auth.ErrInvalidUsername},
		{"username too long", string(make([]byte, 65)), "", applicantPassword, auth.ErrInvalidUsername},
		{"empty password", "applicant", "", "", auth.ErrWeakPassword},
		{"password too short", "applicant", "", "short", auth.ErrWeakPassword},
		// bcrypt silently ignores everything past 72 bytes, so accepting a longer
		// password would be accepting one weaker than it looks.
		{"password past bcrypt's limit", "applicant", "", string(make([]byte, 73)), auth.ErrWeakPassword},
		{"malformed email", "applicant", "not-an-address", applicantPassword, auth.ErrInvalidEmail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mgr.Register(context.Background(), tc.username, tc.email, tc.passwrd)
			if !errors.Is(err, tc.want) {
				t.Errorf("Register error = %v, want %v", err, tc.want)
			}
		})
	}
}

// A rejected application must leave nothing behind.
func TestRegisterCreatesNothingWhenItRejects(t *testing.T) {
	mgr, handle := newAuthDB(t)

	if _, err := mgr.Register(context.Background(), "applicant", "", "short"); err == nil {
		t.Fatal("Register accepted a five-character password")
	}
	var count int
	if err := handle.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Errorf("users count = %d, want 0 after a rejected application", count)
	}
}

// Sessions minted before Login recorded role and status carry only user_id.
// Current must recover the missing fields from the database, or the upgrade
// would sign out everyone who was already logged in.
func TestCurrentRecoversPreUpgradeSession(t *testing.T) {
	mgr, handle := newAuthDB(t)
	bootstrapAdmin(t, mgr)

	// A store over the same database and signing key, used to mint a session in
	// the old shape. NewManager derives its key the same way.
	key := sha256.Sum256([]byte("test-secret"))
	store := auth.NewSQLiteStore(handle, &sessions.Options{
		Path: "/", MaxAge: 3600, HttpOnly: true,
	}, key[:])

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	session, err := store.Get(req, auth.SessionName)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	// The pre-upgrade session shape: an id and nothing else.
	session.Values["user_id"] = int64(1)
	if err := session.Save(req, rec); err != nil {
		t.Fatalf("save the old session: %v", err)
	}

	next := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		next.AddCookie(c)
	}
	who, ok := mgr.Current(next)
	if !ok {
		t.Fatal("a session from before the upgrade stopped working")
	}
	if who.UserID != 1 {
		t.Errorf("UserID = %d, want 1", who.UserID)
	}
	if who.Role != auth.RoleAdmin || who.Status != auth.StatusApproved {
		t.Errorf("recovered identity = (%q, %q), want (%q, %q)",
			who.Role, who.Status, auth.RoleAdmin, auth.StatusApproved)
	}
}

// Without a session there is no identity to report — and no accessor may invent
// one, since stage 03's gate reads them.
func TestAccessorsRejectAnAnonymousRequest(t *testing.T) {
	mgr, cleanup := newManager(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	if _, ok := mgr.UserID(req); ok {
		t.Error("UserID reported a user for an anonymous request")
	}
	if role, ok := mgr.Role(req); ok || role != "" {
		t.Errorf("Role = (%q, %v), want (\"\", false)", role, ok)
	}
	if status, ok := mgr.Status(req); ok || status != "" {
		t.Errorf("Status = (%q, %v), want (\"\", false)", status, ok)
	}
}
