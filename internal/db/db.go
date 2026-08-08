// Package db opens the SQLite database and owns the schema.
//
// The driver is modernc.org/sqlite — a pure-Go port, so the image needs no gcc
// and the binary is built with CGO_ENABLED=0. WAL mode is required: the
// litestream sidecar replicates by following the WAL.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// pragmas are applied per connection via the DSN.
//
//	journal_mode(WAL) — litestream replicates the WAL; required.
//	busy_timeout      — wait rather than fail when the writer holds the lock.
//	foreign_keys      — enforce the jobs -> users/voices references.
//	synchronous       — NORMAL is the documented safe pairing with WAL.
const pragmas = "_pragma=journal_mode(WAL)" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)" +
	"&_pragma=synchronous(NORMAL)"

// Open opens (creating if needed) the SQLite database at path and verifies the
// connection. The caller closes the returned *sql.DB.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}

	handle, err := sql.Open("sqlite", "file:"+path+"?"+pragmas)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	// SQLite takes one writer at a time; a small pool plus busy_timeout keeps
	// concurrent readers cheap without lock storms from the background worker.
	handle.SetMaxOpenConns(4)
	handle.SetMaxIdleConns(4)

	if err := handle.Ping(); err != nil {
		handle.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	return handle, nil
}

// schema is the full data model (project-brief.md §5). Every statement is
// idempotent, so Migrate is safe to run on every boot.
const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT    NOT NULL UNIQUE,
	password_hash TEXT    NOT NULL,
	created_at    TEXT    NOT NULL DEFAULT (datetime('now')),
	role          TEXT    NOT NULL DEFAULT 'user'
	              CHECK (role IN ('admin','user')),
	status        TEXT    NOT NULL DEFAULT 'pending'
	              CHECK (status IN ('approved','pending','disabled')),
	email         TEXT
);

CREATE TABLE IF NOT EXISTS voices (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	kind           TEXT    NOT NULL CHECK (kind IN ('stock','cloned')),
	name           TEXT    NOT NULL,
	model          TEXT,
	license_label  TEXT,
	reference_path TEXT,
	created_at     TEXT    NOT NULL DEFAULT (datetime('now')),
	owner_id       INTEGER REFERENCES users(id) ON DELETE SET NULL,
	is_global      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS voice_assignments (
	voice_id INTEGER NOT NULL REFERENCES voices(id) ON DELETE CASCADE,
	user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	UNIQUE (voice_id, user_id)
);

CREATE TABLE IF NOT EXISTS jobs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	voice_id    INTEGER REFERENCES voices(id) ON DELETE SET NULL,
	text        TEXT    NOT NULL,
	language    TEXT,
	params_json TEXT,
	model          TEXT,
	alignment_json TEXT,
	status      TEXT    NOT NULL DEFAULT 'queued'
	            CHECK (status IN ('queued','submitted','in_progress','ready','failed')),
	runpod_id   TEXT,
	audio_path  TEXT,
	format      TEXT,
	sample_rate INTEGER,
	delay_ms    INTEGER,
	exec_ms     INTEGER,
	error       TEXT,
	attempts    INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
	updated_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS access_requests (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT    NOT NULL,
	email         TEXT,
	password_hash TEXT    NOT NULL,
	status        TEXT    NOT NULL DEFAULT 'pending'
	              CHECK (status IN ('pending','approved','denied')),
	decided_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at    TEXT    NOT NULL DEFAULT (datetime('now')),
	decided_at    TEXT
);

CREATE INDEX IF NOT EXISTS jobs_status_created_idx ON jobs (status, created_at);
CREATE INDEX IF NOT EXISTS jobs_user_created_idx   ON jobs (user_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_runpod_id_idx ON jobs (runpod_id) WHERE runpod_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS access_requests_status_created_idx ON access_requests (status, created_at);
`

// Migrate creates the schema if it is not already present. It also drops the
// vestigial voices.reference_public_url column carried over from Goal 2's
// public-URL reference design — Goal 3 stores reference bytes on a volume and
// base64-encodes them inline, so the column is unused. On a fresh database the
// drop is a no-op.
func Migrate(ctx context.Context, handle *sql.DB) error {
	if _, err := handle.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := dropColumnIfPresent(ctx, handle, "voices", "reference_public_url"); err != nil {
		return fmt.Errorf("migrate: drop reference_public_url: %w", err)
	}
	// jobs.attempts arrived with the submission worker; databases created before
	// it need the column added rather than recreated.
	if err := addColumnIfMissing(ctx, handle, "jobs", "attempts",
		"INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate: add jobs.attempts: %w", err)
	}
	// jobs.model records which model rendered a take, so a library of WAVs
	// stays attributable once this rack runs more than one. Rows queued before
	// the column existed were all rendered by the same single model, so the
	// backfill states that rather than leaving them unattributed. The literal
	// is deliberate: a migration must keep meaning even when jobs.DefaultModel
	// later changes.
	if err := addColumnIfMissing(ctx, handle, "jobs", "model", "TEXT"); err != nil {
		return fmt.Errorf("migrate: add jobs.model: %w", err)
	}
	if _, err := handle.ExecContext(ctx,
		`UPDATE jobs SET model = 'MOSS-TTS v1.5' WHERE model IS NULL OR model = ''`); err != nil {
		return fmt.Errorf("migrate: backfill jobs.model: %w", err)
	}
	// jobs.alignment_json stores the optional word_timings block the serverless
	// worker attaches, so the player can track the spoken word from real forced
	// alignment instead of proportional interpolation. NULL/empty means the
	// worker omitted it (older build, streaming, alignment failed) and the player
	// falls back — it is never required.
	if err := addColumnIfMissing(ctx, handle, "jobs", "alignment_json", "TEXT"); err != nil {
		return fmt.Errorf("migrate: add jobs.alignment_json: %w", err)
	}
	// users.role and users.status turn the single bootstrapped account into a
	// population: role decides who may administer, status decides who reaches
	// the studio at all. email is optional because the bootstrapped admin never
	// supplied one.
	if err := addColumnIfMissing(ctx, handle, "users", "role",
		"TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin','user'))"); err != nil {
		return fmt.Errorf("migrate: add users.role: %w", err)
	}
	if err := addColumnIfMissing(ctx, handle, "users", "status",
		"TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('approved','pending','disabled'))"); err != nil {
		return fmt.Errorf("migrate: add users.status: %w", err)
	}
	if err := addColumnIfMissing(ctx, handle, "users", "email", "TEXT"); err != nil {
		return fmt.Errorf("migrate: add users.email: %w", err)
	}
	// Those defaults are right for an applicant and wrong for the one account
	// that already exists: they would leave the bootstrapped admin 'pending',
	// locked out of the app it administers, with nobody able to approve it. The
	// lowest id is that admin — auth.Bootstrap only ever writes into an empty
	// table — so it is restored on every pass. Re-running is harmless, and
	// scoping to MIN(id) means no later account is ever promoted by a restart.
	if _, err := handle.ExecContext(ctx,
		`UPDATE users SET role = 'admin', status = 'approved' WHERE id = (SELECT MIN(id) FROM users)`); err != nil {
		return fmt.Errorf("migrate: restore bootstrap admin: %w", err)
	}
	// voices gains ownership. owner_id is the account that cloned the card and
	// stays nullable: stock cards have no owner, and deleting an account must
	// orphan its cards rather than destroy them, hence ON DELETE SET NULL. With
	// is_global alongside it, visibility is `owner_id = ? OR is_global = 1` —
	// answerable from the voices row alone, no join.
	if err := addColumnIfMissing(ctx, handle, "voices", "owner_id",
		"INTEGER REFERENCES users(id) ON DELETE SET NULL"); err != nil {
		return fmt.Errorf("migrate: add voices.owner_id: %w", err)
	}
	hadIsGlobal, err := columnExists(ctx, handle, "voices", "is_global")
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := addColumnIfMissing(ctx, handle, "voices", "is_global",
		"INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate: add voices.is_global: %w", err)
	}
	if !hadIsGlobal {
		// Stock cards are the shared library every account starts from, so they
		// are promoted exactly once: at the moment the flag appears. Doing it on
		// every pass would silently overrule an admin who later un-globals one.
		if _, err := handle.ExecContext(ctx,
			`UPDATE voices SET is_global = 1 WHERE kind = 'stock'`); err != nil {
			return fmt.Errorf("migrate: backfill voices.is_global: %w", err)
		}
	}
	// voice_assignments supersedes owner_id as the access-control source. Copy
	// legacy ownership into the junction table so cards created between stages
	// 01 and 04 stay visible after upgrade. INSERT OR IGNORE makes the backfill
	// idempotent and preserves any additional many-to-many assignments.
	if _, err := handle.ExecContext(ctx, `
		INSERT OR IGNORE INTO voice_assignments (voice_id, user_id)
		SELECT id, owner_id FROM voices WHERE owner_id IS NOT NULL`); err != nil {
		return fmt.Errorf("migrate: backfill voice assignments: %w", err)
	}
	// jobs needs no change: user_id has been NOT NULL since the table was
	// created and every query already filters on it, so outputs are isolated by
	// construction. Recorded here so later work does not re-derive it.
	return nil
}

// addColumnIfMissing adds column to table when it is absent, so an existing
// database picks up a new field without a rebuild. CREATE TABLE IF NOT EXISTS
// silently skips a table that already exists, which is why new columns need
// this rather than an edit to the schema constant alone. table, column and
// definition are compile-time constants here (never caller input), so
// interpolating them into the DDL statement is safe.
func addColumnIfMissing(ctx context.Context, handle *sql.DB, table, column, definition string) error {
	exists, err := columnExists(ctx, handle, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := handle.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition)); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// columnExists reports whether a column is already on a table. Migrate uses it
// directly when a backfill must run only on the pass that introduces a column,
// which addColumnIfMissing alone cannot express — it succeeds identically
// whether it added the column or found it.
func columnExists(ctx context.Context, handle *sql.DB, table, column string) (bool, error) {
	var count int
	if err := handle.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	return count > 0, nil
}

// dropColumnIfPresent removes column from table when it exists. table and
// column are compile-time constants here (never caller input), so interpolating
// them into the DDL statement is safe.
func dropColumnIfPresent(ctx context.Context, handle *sql.DB, table, column string) error {
	var count int
	if err := handle.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil {
		return fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	if count == 0 {
		return nil
	}
	if _, err := handle.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, table, column)); err != nil {
		return fmt.Errorf("drop %s.%s: %w", table, column, err)
	}
	return nil
}
