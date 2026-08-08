package auth

// This file owns the access_requests table: the queue of people asking for an
// account. It sits in the auth package rather than one of its own because
// approving a request creates a users row in the same transaction, and
// splitting that transaction across two packages would cost more than the
// separation buys.
//
// The HTTP surface is not here. Stage 06 wires the admin routes and stage 07
// the public application form; this is the store they both call.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Access request states, as stored in access_requests.status. Deliberately
// distinct from the users.status set: a request is pending/approved/denied, a
// user is approved/pending/disabled, and the two overlap without meaning the
// same thing.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDenied   = "denied"
)

var (
	ErrNoSuchRequest     = errors.New("no such access request")
	ErrRequestNotPending = errors.New("that access request has already been decided")
	ErrRequestPending    = errors.New("an application for that username is already awaiting review")
)

// AccessRequest is one application. The submitted password hash is deliberately
// absent from this struct — nothing outside this store has any use for it, and
// leaving it off means it cannot be serialized to a client by accident.
type AccessRequest struct {
	ID        int64
	Username  string
	Email     string
	Status    string
	DecidedBy int64 // 0 while undecided
	CreatedAt string
	DecidedAt string
}

// AccessRequests owns the access_requests table.
type AccessRequests struct {
	db *sql.DB
}

func NewAccessRequests(db *sql.DB) *AccessRequests {
	return &AccessRequests{db: db}
}

// rowQuerier is the part of *sql.DB and *sql.Tx this file needs, so a lookup
// can run inside an open transaction or outside one without duplicating it.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const accessRequestColumns = `id, username, COALESCE(email, ''), status,
	COALESCE(decided_by, 0), created_at, COALESCE(decided_at, '')`

// Create records an application. It validates and hashes exactly as Register
// does, so a request that is accepted here can always become an account later
// without asking for the password again. It grants nothing.
func (s *AccessRequests) Create(ctx context.Context, username, email, password string) (int64, error) {
	username, email, err := validateApplication(username, email, password)
	if err != nil {
		return 0, err
	}

	// An account already holding the name means this request could never be
	// approved, so it is refused now rather than at the admin's desk. Unlike
	// Register there is no UNIQUE constraint to lean on — access_requests
	// deliberately allows a denied applicant to reapply — so the check is
	// explicit.
	var taken int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM users WHERE username = ?", username).Scan(&taken); err != nil {
		return 0, fmt.Errorf("access request: check username: %w", err)
	}
	if taken > 0 {
		return 0, ErrUsernameTaken
	}
	// One open application per name is enough; a second would give an admin two
	// identical rows to decide and a way to spam the queue.
	var waiting int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM access_requests WHERE username = ? AND status = ?",
		username, RequestPending).Scan(&waiting); err != nil {
		return 0, fmt.Errorf("access request: check pending: %w", err)
	}
	if waiting > 0 {
		return 0, ErrRequestPending
	}

	hash, err := HashPassword(password)
	if err != nil {
		return 0, fmt.Errorf("access request: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO access_requests (username, email, password_hash, status) VALUES (?, ?, ?, ?)`,
		username, nullable(email), hash, RequestPending)
	if err != nil {
		return 0, fmt.Errorf("access request: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("access request: new id: %w", err)
	}
	return id, nil
}

// List returns applications newest first. status filters the result; "" returns
// every request, decided or not, since the admin queue shows history as well as
// work to do.
func (s *AccessRequests) List(ctx context.Context, status string) ([]AccessRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+accessRequestColumns+`
		 FROM access_requests
		 WHERE ? = '' OR status = ?
		 ORDER BY created_at DESC, id DESC`, status, status)
	if err != nil {
		return nil, fmt.Errorf("list access requests: %w", err)
	}
	defer rows.Close()

	var out []AccessRequest
	for rows.Next() {
		var req AccessRequest
		if err := rows.Scan(&req.ID, &req.Username, &req.Email, &req.Status,
			&req.DecidedBy, &req.CreatedAt, &req.DecidedAt); err != nil {
			return nil, fmt.Errorf("list access requests: scan: %w", err)
		}
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list access requests: %w", err)
	}
	return out, nil
}

// Get returns one request, or ErrNoSuchRequest.
func (s *AccessRequests) Get(ctx context.Context, id int64) (AccessRequest, error) {
	return scanRequest(s.db, ctx, id)
}

func scanRequest(q rowQuerier, ctx context.Context, id int64) (AccessRequest, error) {
	var req AccessRequest
	err := q.QueryRowContext(ctx,
		`SELECT `+accessRequestColumns+` FROM access_requests WHERE id = ?`, id).
		Scan(&req.ID, &req.Username, &req.Email, &req.Status,
			&req.DecidedBy, &req.CreatedAt, &req.DecidedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AccessRequest{}, ErrNoSuchRequest
	case err != nil:
		return AccessRequest{}, fmt.Errorf("get access request: %w", err)
	}
	return req, nil
}

// Approve turns a pending request into an approved account and returns the new
// user's id. Both halves happen in one transaction, and the request is claimed
// *first*: the `status = 'pending'` in the WHERE clause is what two admins
// clicking at the same moment collide on, so the loser gets
// ErrRequestNotPending instead of a second account.
//
// The account is built from the credentials captured when the application was
// made, so the applicant's password still works. It is created 'approved' —
// that is what approving means — and always role 'user'; making an admin is a
// separate, deliberate act.
func (s *AccessRequests) Approve(ctx context.Context, id, decidedBy int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("approve: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	claimed, err := claim(ctx, tx, id, RequestApproved, decidedBy)
	if err != nil {
		return 0, err
	}
	if !claimed {
		return 0, undecidable(ctx, tx, id)
	}

	var username, hash string
	var email sql.Null[string]
	if err := tx.QueryRowContext(ctx,
		"SELECT username, email, password_hash FROM access_requests WHERE id = ?", id).
		Scan(&username, &email, &hash); err != nil {
		return 0, fmt.Errorf("approve: read request: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, email, role, status) VALUES (?, ?, ?, ?, ?)`,
		username, hash, nullable(email.V), RoleUser, StatusApproved)
	if err != nil {
		// Someone took the name between the application and the decision. The
		// rollback leaves the request pending so an admin can deny it instead.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, ErrUsernameTaken
		}
		return 0, fmt.Errorf("approve: insert user: %w", err)
	}
	userID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("approve: new user id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("approve: commit: %w", err)
	}
	return userID, nil
}

// Deny marks a pending request denied. It creates nothing, so the single
// conditional UPDATE is already atomic and needs no transaction.
func (s *AccessRequests) Deny(ctx context.Context, id, decidedBy int64) error {
	claimed, err := claim(ctx, s.db, id, RequestDenied, decidedBy)
	if err != nil {
		return err
	}
	if !claimed {
		return undecidable(ctx, s.db, id)
	}
	return nil
}

// execer is the part of *sql.DB and *sql.Tx that claim needs.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// claim moves a request out of 'pending' and reports whether it was this caller
// who moved it. Only a pending request can be decided, which is what makes a
// second decision a no-op rather than an overwrite.
func claim(ctx context.Context, q execer, id int64, to string, decidedBy int64) (bool, error) {
	res, err := q.ExecContext(ctx,
		`UPDATE access_requests
		 SET status = ?, decided_by = ?, decided_at = datetime('now')
		 WHERE id = ? AND status = ?`, to, nullableID(decidedBy), id, RequestPending)
	if err != nil {
		return false, fmt.Errorf("decide access request: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("decide access request: %w", err)
	}
	return affected == 1, nil
}

// undecidable explains why a claim matched nothing: either the request is gone
// or somebody already decided it.
func undecidable(ctx context.Context, q rowQuerier, id int64) error {
	if _, err := scanRequest(q, ctx, id); err != nil {
		return err
	}
	return ErrRequestNotPending
}
