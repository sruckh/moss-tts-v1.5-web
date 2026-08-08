package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// The schema as it stood before multi-user: a single bootstrapped account, no
// role or status, voices with no owner. Databases in this shape exist in the
// wild, so every assertion below is made twice — once on a fresh install and
// once on a database upgraded from here — and the two must agree.
const preMultiUserSchema = `
CREATE TABLE users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now')));
CREATE TABLE voices (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL CHECK (kind IN ('stock','cloned')),
	name TEXT NOT NULL, model TEXT, license_label TEXT, reference_path TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now')));
CREATE TABLE jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	voice_id INTEGER REFERENCES voices(id) ON DELETE SET NULL,
	text TEXT NOT NULL, language TEXT, params_json TEXT,
	status TEXT NOT NULL DEFAULT 'queued'
		CHECK (status IN ('queued','submitted','in_progress','ready','failed')),
	runpod_id TEXT, audio_path TEXT, format TEXT, sample_rate INTEGER,
	delay_ms INTEGER, exec_ms INTEGER, error TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at TEXT NOT NULL DEFAULT (datetime('now')));`

// openPreMultiUserDB returns an un-migrated database in the pre-multi-user
// shape, for tests that need to seed legacy rows before Migrate runs.
func openPreMultiUserDB(t *testing.T) *sql.DB {
	t.Helper()
	handle, err := Open(filepath.Join(t.TempDir(), "timbre.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if _, err := handle.ExecContext(context.Background(), preMultiUserSchema); err != nil {
		t.Fatalf("seed pre-multi-user schema: %v", err)
	}
	return handle
}

func openUpgradedDB(t *testing.T) *sql.DB {
	t.Helper()
	handle := openPreMultiUserDB(t)
	if err := Migrate(context.Background(), handle); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return handle
}

type columnInfo struct {
	typ      string
	notNull  int
	defValue sql.Null[string]
}

func columnInfoFor(t *testing.T, handle *sql.DB, table, column string) columnInfo {
	t.Helper()
	var info columnInfo
	err := handle.QueryRow(
		`SELECT type, "notnull", dflt_value FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&info.typ, &info.notNull, &info.defValue)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("%s.%s is missing", table, column)
	}
	if err != nil {
		t.Fatalf("inspect %s.%s: %v", table, column, err)
	}
	return info
}

// Every new column carries a default, so rows written before multi-user existed
// migrate without a rebuild. A fresh install gets them from the schema constant
// and an existing one from ALTER TABLE; this pins that both routes land on the
// same column definitions.
func TestMigrateAddsMultiUserColumns(t *testing.T) {
	shapes := []struct {
		name string
		open func(*testing.T) *sql.DB
	}{
		{"fresh", openTestDB},
		{"upgraded", openUpgradedDB},
	}
	columns := []struct {
		table, column, typ string
		notNull            int
		def                string // "" means no default
	}{
		{"users", "role", "TEXT", 1, "'user'"},
		{"users", "status", "TEXT", 1, "'pending'"},
		{"users", "email", "TEXT", 0, ""},
		{"voices", "owner_id", "INTEGER", 0, ""},
		{"voices", "is_global", "INTEGER", 1, "0"},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			handle := shape.open(t)
			for _, want := range columns {
				got := columnInfoFor(t, handle, want.table, want.column)
				if got.typ != want.typ {
					t.Errorf("%s.%s type = %q, want %q", want.table, want.column, got.typ, want.typ)
				}
				if got.notNull != want.notNull {
					t.Errorf("%s.%s notnull = %d, want %d", want.table, want.column, got.notNull, want.notNull)
				}
				if got.defValue.V != want.def {
					t.Errorf("%s.%s default = %q, want %q", want.table, want.column, got.defValue.V, want.def)
				}
			}
		})
	}
}

// The role and status domains are enforced by the database, not only by the Go
// code that will read them in later stages. ALTER TABLE ADD COLUMN carries a
// CHECK constraint, so the upgraded shape must reject the same values as a
// fresh one.
func TestUserRoleAndStatusAreConstrained(t *testing.T) {
	for _, shape := range []struct {
		name string
		open func(*testing.T) *sql.DB
	}{
		{"fresh", openTestDB},
		{"upgraded", openUpgradedDB},
	} {
		t.Run(shape.name, func(t *testing.T) {
			handle := shape.open(t)
			ctx := context.Background()

			if _, err := handle.ExecContext(ctx,
				`INSERT INTO users (username, password_hash, role) VALUES ('wizard', 'x', 'wizard')`); err == nil {
				t.Error("expected the role CHECK constraint to reject 'wizard'")
			}
			if _, err := handle.ExecContext(ctx,
				`INSERT INTO users (username, password_hash, status) VALUES ('limbo', 'x', 'limbo')`); err == nil {
				t.Error("expected the status CHECK constraint to reject 'limbo'")
			}

			if _, err := handle.ExecContext(ctx,
				`INSERT INTO users (username, password_hash) VALUES ('applicant', 'x')`); err != nil {
				t.Fatalf("insert applicant: %v", err)
			}
			var role, status string
			if err := handle.QueryRowContext(ctx,
				`SELECT role, status FROM users WHERE username = 'applicant'`).Scan(&role, &status); err != nil {
				t.Fatalf("read applicant: %v", err)
			}
			// A new account is an applicant until an admin says otherwise.
			if role != "user" || status != "pending" {
				t.Errorf("new user = (%q, %q), want (user, pending)", role, status)
			}
		})
	}
}

// status defaults to 'pending', which would leave the bootstrapped admin locked
// out of the app it administers with nobody able to approve it. Migrate puts it
// back on every pass — and must never hand that promotion to anyone else.
func TestMigrateRestoresBootstrapAdminOnly(t *testing.T) {
	ctx := context.Background()
	handle := openPreMultiUserDB(t)

	if _, err := handle.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash) VALUES
			(1, 'admin', 'x'),
			(2, 'newcomer', 'y')`); err != nil {
		t.Fatalf("seed users: %v", err)
	}

	// Twice: the restore has to survive every boot, not only the first.
	for pass := 1; pass <= 2; pass++ {
		if err := Migrate(ctx, handle); err != nil {
			t.Fatalf("Migrate pass %d: %v", pass, err)
		}
	}

	for _, want := range []struct {
		id           int64
		role, status string
	}{
		{1, "admin", "approved"},
		{2, "user", "pending"},
	} {
		var role, status string
		if err := handle.QueryRowContext(ctx,
			`SELECT role, status FROM users WHERE id = ?`, want.id).Scan(&role, &status); err != nil {
			t.Fatalf("read user %d: %v", want.id, err)
		}
		if role != want.role || status != want.status {
			t.Errorf("user %d = (%q, %q), want (%q, %q)", want.id, role, status, want.role, want.status)
		}
	}
}

// Migrate runs before any account exists on a brand-new install, so the restore
// must be a no-op rather than an error on an empty table.
func TestMigrateRestoreToleratesNoUsers(t *testing.T) {
	handle := openUpgradedDB(t)

	var count int
	if err := handle.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Errorf("users count = %d, want 0", count)
	}
}

// Stock cards are the shared library every account starts from, so the flag is
// backfilled for them — once. A later pass must not overrule an admin who has
// since un-globaled one.
func TestMigrateMakesStockVoicesGlobalExactlyOnce(t *testing.T) {
	ctx := context.Background()
	handle := openPreMultiUserDB(t)

	if _, err := handle.ExecContext(ctx, `
		INSERT INTO voices (id, kind, name) VALUES
			(1, 'stock',  'Aria'),
			(2, 'cloned', 'My own take')`); err != nil {
		t.Fatalf("seed voices: %v", err)
	}

	if err := Migrate(ctx, handle); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, want := range []struct {
		id       int64
		isGlobal int
	}{
		{1, 1},
		{2, 0},
	} {
		var isGlobal int
		var owner sql.Null[int64]
		if err := handle.QueryRowContext(ctx,
			`SELECT is_global, owner_id FROM voices WHERE id = ?`, want.id).Scan(&isGlobal, &owner); err != nil {
			t.Fatalf("read voice %d: %v", want.id, err)
		}
		if isGlobal != want.isGlobal {
			t.Errorf("voice %d is_global = %d, want %d", want.id, isGlobal, want.isGlobal)
		}
		// Nothing that predates ownership can be attributed to an account.
		if owner.Valid {
			t.Errorf("voice %d owner_id = %d, want NULL", want.id, owner.V)
		}
	}

	if _, err := handle.ExecContext(ctx, `UPDATE voices SET is_global = 0 WHERE id = 1`); err != nil {
		t.Fatalf("un-global the stock card: %v", err)
	}
	if err := Migrate(ctx, handle); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var isGlobal int
	if err := handle.QueryRowContext(ctx, `SELECT is_global FROM voices WHERE id = 1`).Scan(&isGlobal); err != nil {
		t.Fatalf("re-read voice 1: %v", err)
	}
	if isGlobal != 0 {
		t.Error("Migrate re-globaled a stock card an admin had already un-globaled")
	}
}

// Losing an account must orphan its cards, not destroy them — hence ON DELETE
// SET NULL rather than the CASCADE jobs uses.
func TestDeletingOwnerOrphansVoiceRatherThanDeletingIt(t *testing.T) {
	handle := openTestDB(t)
	ctx := context.Background()

	if _, err := handle.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash) VALUES (1, 'clara', 'x')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO voices (id, kind, name, owner_id) VALUES (1, 'cloned', 'Clara', 1)`); err != nil {
		t.Fatalf("insert voice: %v", err)
	}
	if _, err := handle.ExecContext(ctx, `DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	var owner sql.Null[int64]
	if err := handle.QueryRowContext(ctx, `SELECT owner_id FROM voices WHERE id = 1`).Scan(&owner); err != nil {
		t.Fatalf("the voice went with its owner: %v", err)
	}
	if owner.Valid {
		t.Errorf("owner_id = %d after the owner was deleted, want NULL", owner.V)
	}
}

// access_requests holds an application from someone who has no account yet, so
// it carries the requested credentials itself rather than referencing users.
func TestMigrateCreatesAccessRequests(t *testing.T) {
	for _, shape := range []struct {
		name string
		open func(*testing.T) *sql.DB
	}{
		{"fresh", openTestDB},
		{"upgraded", openUpgradedDB},
	} {
		t.Run(shape.name, func(t *testing.T) {
			handle := shape.open(t)
			ctx := context.Background()

			for _, column := range []string{
				"id", "username", "email", "password_hash",
				"status", "decided_by", "created_at", "decided_at",
			} {
				columnInfoFor(t, handle, "access_requests", column) // fatal when missing
			}

			if _, err := handle.ExecContext(ctx,
				`INSERT INTO access_requests (username, password_hash) VALUES ('applicant', 'hash')`); err != nil {
				t.Fatalf("insert request: %v", err)
			}
			var status string
			var decidedBy, decidedAt sql.Null[string]
			if err := handle.QueryRowContext(ctx,
				`SELECT status, decided_by, decided_at FROM access_requests WHERE username = 'applicant'`).
				Scan(&status, &decidedBy, &decidedAt); err != nil {
				t.Fatalf("read request: %v", err)
			}
			if status != "pending" {
				t.Errorf("new request status = %q, want pending", status)
			}
			// Undecided until an admin acts on it.
			if decidedBy.Valid || decidedAt.Valid {
				t.Error("a new request arrived already decided")
			}

			if _, err := handle.ExecContext(ctx,
				`INSERT INTO access_requests (username, password_hash, status)
				 VALUES ('other', 'hash', 'maybe')`); err == nil {
				t.Error("expected the status CHECK constraint to reject 'maybe'")
			}
		})
	}
}

func TestMigrateCreatesVoiceAssignments(t *testing.T) {
	shapes := []struct {
		name string
		open func(*testing.T) *sql.DB
	}{
		{"fresh", openTestDB},
		{"upgraded", openUpgradedDB},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			handle := shape.open(t)
			userResult, err := handle.Exec(`INSERT INTO users (username, password_hash) VALUES ('assigned', 'hash')`)
			if err != nil {
				t.Fatalf("insert user: %v", err)
			}
			userID, err := userResult.LastInsertId()
			if err != nil {
				t.Fatalf("user id: %v", err)
			}
			voiceResult, err := handle.Exec(`
				INSERT INTO voices (kind, name, owner_id) VALUES ('cloned', 'Legacy clone', ?)`, userID)
			if err != nil {
				t.Fatalf("insert voice: %v", err)
			}
			voiceID, err := voiceResult.LastInsertId()
			if err != nil {
				t.Fatalf("voice id: %v", err)
			}

			if err := Migrate(context.Background(), handle); err != nil {
				t.Fatalf("Migrate backfill: %v", err)
			}
			if err := Migrate(context.Background(), handle); err != nil {
				t.Fatalf("Migrate idempotent: %v", err)
			}
			var count int
			if err := handle.QueryRow(`
				SELECT COUNT(*) FROM voice_assignments WHERE voice_id = ? AND user_id = ?`,
				voiceID, userID).Scan(&count); err != nil {
				t.Fatalf("assignment count: %v", err)
			}
			if count != 1 {
				t.Fatalf("assignment count = %d, want 1", count)
			}

			if _, err := handle.Exec(`DELETE FROM users WHERE id = ?`, userID); err != nil {
				t.Fatalf("delete user: %v", err)
			}
			if err := handle.QueryRow(`SELECT COUNT(*) FROM voice_assignments WHERE voice_id = ?`, voiceID).Scan(&count); err != nil {
				t.Fatalf("assignment count after user delete: %v", err)
			}
			if count != 0 {
				t.Fatalf("assignment count after user delete = %d, want 0", count)
			}
			if err := handle.QueryRow(`SELECT COUNT(*) FROM voices WHERE id = ?`, voiceID).Scan(&count); err != nil {
				t.Fatalf("voice count after user delete: %v", err)
			}
			if count != 1 {
				t.Fatalf("voice count after user delete = %d, want 1", count)
			}
		})
	}
}

// jobs was already scoped by user_id, so stage 01 deliberately left it alone.
// This pins that decision: relaxing user_id later would silently un-isolate
// every rendered take.
func TestJobsUserScopingIsUnchanged(t *testing.T) {
	handle := openTestDB(t)

	if got := columnInfoFor(t, handle, "jobs", "user_id"); got.notNull != 1 {
		t.Error("jobs.user_id is nullable; outputs are no longer isolated by construction")
	}
	for _, absent := range []string{"owner_id", "is_global"} {
		var count int
		if err := handle.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name = ?`, absent).Scan(&count); err != nil {
			t.Fatalf("inspect jobs.%s: %v", absent, err)
		}
		if count != 0 {
			t.Errorf("jobs grew a %s column; user_id is already the isolation key", absent)
		}
	}
}
