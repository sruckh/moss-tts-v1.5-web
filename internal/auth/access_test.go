package auth_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sruckh/timbre/internal/auth"
)

// newRequests returns the store, the handle behind it, and the id of an admin
// to attribute decisions to — decided_by is a foreign key, so the tests need a
// real one.
func newRequests(t *testing.T) (*auth.AccessRequests, *sql.DB, int64) {
	t.Helper()

	mgr, handle := newAuthDB(t)
	bootstrapAdmin(t, mgr)

	var adminID int64
	if err := handle.QueryRow("SELECT id FROM users WHERE username = 'admin'").Scan(&adminID); err != nil {
		t.Fatalf("find the admin: %v", err)
	}
	return auth.NewAccessRequests(handle), handle, adminID
}

func TestCreateRecordsAPendingRequest(t *testing.T) {
	store, _, _ := newRequests(t)
	ctx := context.Background()

	id, err := store.Create(ctx, "applicant", "applicant@example.com", applicantPassword)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if req.Username != "applicant" || req.Email != "applicant@example.com" {
		t.Errorf("request = %+v, want the submitted username and email", req)
	}
	if req.Status != auth.RequestPending {
		t.Errorf("status = %q, want %q", req.Status, auth.RequestPending)
	}
	// Nobody has decided it yet, and the zero values say so.
	if req.DecidedBy != 0 || req.DecidedAt != "" {
		t.Errorf("a new request arrived decided: by=%d at=%q", req.DecidedBy, req.DecidedAt)
	}
	if req.CreatedAt == "" {
		t.Error("request has no created_at")
	}
}

// A name already held by an account could never be approved, so it is refused
// at application time rather than at the admin's desk.
func TestCreateRejectsAUsernameAnAccountHolds(t *testing.T) {
	store, _, _ := newRequests(t)

	_, err := store.Create(context.Background(), "admin", "", applicantPassword)
	if !errors.Is(err, auth.ErrUsernameTaken) {
		t.Errorf("Create error = %v, want ErrUsernameTaken", err)
	}
}

// One open application per name: a second would give the admin two identical
// rows to decide and a way to flood the queue.
func TestCreateRejectsASecondOpenApplication(t *testing.T) {
	store, _, _ := newRequests(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, "applicant", "", applicantPassword); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := store.Create(ctx, "applicant", "", applicantPassword)
	if !errors.Is(err, auth.ErrRequestPending) {
		t.Errorf("second Create error = %v, want ErrRequestPending", err)
	}
}

// Being turned down once is not a life sentence — the schema deliberately keeps
// no unique constraint on the name so a denied applicant can ask again.
func TestDeniedApplicantMayReapply(t *testing.T) {
	store, _, adminID := newRequests(t)
	ctx := context.Background()

	first, err := store.Create(ctx, "applicant", "", applicantPassword)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := store.Deny(ctx, first, adminID); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if _, err := store.Create(ctx, "applicant", "", applicantPassword); err != nil {
		t.Errorf("a denied applicant could not reapply: %v", err)
	}
}

// Create and Register must agree, or a request could be accepted here and then
// fail to become an account.
func TestCreateValidatesLikeRegister(t *testing.T) {
	store, _, _ := newRequests(t)
	ctx := context.Background()

	for name, tc := range map[string]struct {
		username, email, password string
		want                      error
	}{
		"short username": {"ab", "", applicantPassword, auth.ErrInvalidUsername},
		"short password": {"applicant", "", "short", auth.ErrWeakPassword},
		"bad email":      {"applicant", "nope", applicantPassword, auth.ErrInvalidEmail},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Create(ctx, tc.username, tc.email, tc.password); !errors.Is(err, tc.want) {
				t.Errorf("Create error = %v, want %v", err, tc.want)
			}
		})
	}
}

// Approving builds the account from the credentials captured at application
// time, so the applicant's password still works and they are not asked again.
func TestApproveCreatesAnApprovedUser(t *testing.T) {
	store, handle, adminID := newRequests(t)
	ctx := context.Background()

	id, err := store.Create(ctx, "applicant", "applicant@example.com", applicantPassword)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	userID, err := store.Approve(ctx, id, adminID)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	var username, hash, role, status string
	var email sql.Null[string]
	if err := handle.QueryRowContext(ctx,
		"SELECT username, password_hash, email, role, status FROM users WHERE id = ?", userID).
		Scan(&username, &hash, &email, &role, &status); err != nil {
		t.Fatalf("read the new user: %v", err)
	}
	if username != "applicant" {
		t.Errorf("username = %q, want applicant", username)
	}
	if !auth.CheckPassword(hash, applicantPassword) {
		t.Error("the approved account does not accept the password that was applied with")
	}
	if !email.Valid || email.V != "applicant@example.com" {
		t.Errorf("email = %+v, want the applied address", email)
	}
	// Approved on arrival — that is what approving means — but never an admin.
	if role != auth.RoleUser || status != auth.StatusApproved {
		t.Errorf("new user = (%q, %q), want (%q, %q)", role, status, auth.RoleUser, auth.StatusApproved)
	}

	req, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if req.Status != auth.RequestApproved || req.DecidedBy != adminID || req.DecidedAt == "" {
		t.Errorf("decided request = %+v, want approved by %d with a timestamp", req, adminID)
	}
}

// The audit: a decision happens once. Two admins acting at the same moment must
// not produce two accounts.
func TestApproveIsAtomicPerRequest(t *testing.T) {
	store, handle, adminID := newRequests(t)
	ctx := context.Background()

	id, err := store.Create(ctx, "applicant", "", applicantPassword)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Approve(ctx, id, adminID); err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	if _, err := store.Approve(ctx, id, adminID); !errors.Is(err, auth.ErrRequestNotPending) {
		t.Errorf("second Approve error = %v, want ErrRequestNotPending", err)
	}

	var count int
	if err := handle.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM users WHERE username = 'applicant'").Scan(&count); err != nil {
		t.Fatalf("count applicants: %v", err)
	}
	if count != 1 {
		t.Errorf("approving twice produced %d accounts, want 1", count)
	}
}

func TestDenyMarksTheRequestAndCreatesNoUser(t *testing.T) {
	store, handle, adminID := newRequests(t)
	ctx := context.Background()

	id, err := store.Create(ctx, "applicant", "", applicantPassword)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Deny(ctx, id, adminID); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	req, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if req.Status != auth.RequestDenied || req.DecidedBy != adminID || req.DecidedAt == "" {
		t.Errorf("denied request = %+v, want denied by %d with a timestamp", req, adminID)
	}

	var count int
	if err := handle.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM users WHERE username = 'applicant'").Scan(&count); err != nil {
		t.Fatalf("count applicants: %v", err)
	}
	if count != 0 {
		t.Errorf("denying created %d accounts, want 0", count)
	}
}

// Only a pending request can be decided, whichever way the first decision went.
func TestADecidedRequestCannotBeDecidedAgain(t *testing.T) {
	store, _, adminID := newRequests(t)
	ctx := context.Background()

	id, err := store.Create(ctx, "applicant", "", applicantPassword)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Deny(ctx, id, adminID); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if _, err := store.Approve(ctx, id, adminID); !errors.Is(err, auth.ErrRequestNotPending) {
		t.Errorf("Approve after Deny = %v, want ErrRequestNotPending", err)
	}
	if err := store.Deny(ctx, id, adminID); !errors.Is(err, auth.ErrRequestNotPending) {
		t.Errorf("second Deny = %v, want ErrRequestNotPending", err)
	}
}

func TestDecidingAnUnknownRequest(t *testing.T) {
	store, _, adminID := newRequests(t)
	ctx := context.Background()

	if _, err := store.Approve(ctx, 404, adminID); !errors.Is(err, auth.ErrNoSuchRequest) {
		t.Errorf("Approve error = %v, want ErrNoSuchRequest", err)
	}
	if err := store.Deny(ctx, 404, adminID); !errors.Is(err, auth.ErrNoSuchRequest) {
		t.Errorf("Deny error = %v, want ErrNoSuchRequest", err)
	}
	if _, err := store.Get(ctx, 404); !errors.Is(err, auth.ErrNoSuchRequest) {
		t.Errorf("Get error = %v, want ErrNoSuchRequest", err)
	}
}

// The admin queue shows work to do and history alike, so List filters rather
// than assuming one or the other.
func TestListFiltersByStatus(t *testing.T) {
	store, _, adminID := newRequests(t)
	ctx := context.Background()

	waiting, err := store.Create(ctx, "waiting", "", applicantPassword)
	if err != nil {
		t.Fatalf("Create waiting: %v", err)
	}
	turnedDown, err := store.Create(ctx, "turned-down", "", applicantPassword)
	if err != nil {
		t.Fatalf("Create turned-down: %v", err)
	}
	if err := store.Deny(ctx, turnedDown, adminID); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	for name, tc := range map[string]struct {
		filter string
		want   int
	}{
		"all":     {"", 2},
		"pending": {auth.RequestPending, 1},
		"denied":  {auth.RequestDenied, 1},
		"none":    {auth.RequestApproved, 0},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := store.List(ctx, tc.filter)
			if err != nil {
				t.Fatalf("List(%q): %v", tc.filter, err)
			}
			if len(got) != tc.want {
				t.Fatalf("List(%q) returned %d, want %d", tc.filter, len(got), tc.want)
			}
		})
	}

	only, err := store.List(ctx, auth.RequestPending)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if only[0].ID != waiting {
		t.Errorf("pending list holds request %d, want %d", only[0].ID, waiting)
	}
}
