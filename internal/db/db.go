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
	created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS voices (
	id                   INTEGER PRIMARY KEY AUTOINCREMENT,
	kind                 TEXT    NOT NULL CHECK (kind IN ('stock','cloned')),
	name                 TEXT    NOT NULL,
	model                TEXT,
	license_label        TEXT,
	reference_path       TEXT,
	reference_public_url TEXT,
	created_at           TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS jobs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	voice_id    INTEGER REFERENCES voices(id) ON DELETE SET NULL,
	text        TEXT    NOT NULL,
	language    TEXT,
	params_json TEXT,
	status      TEXT    NOT NULL DEFAULT 'queued'
	            CHECK (status IN ('queued','submitted','in_progress','ready','failed')),
	runpod_id   TEXT,
	audio_path  TEXT,
	format      TEXT,
	sample_rate INTEGER,
	delay_ms    INTEGER,
	exec_ms     INTEGER,
	error       TEXT,
	created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
	updated_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS jobs_status_created_idx ON jobs (status, created_at);
CREATE INDEX IF NOT EXISTS jobs_user_created_idx   ON jobs (user_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_runpod_id_idx ON jobs (runpod_id) WHERE runpod_id IS NOT NULL;
`

// Migrate creates the schema if it is not already present.
func Migrate(ctx context.Context, handle *sql.DB) error {
	if _, err := handle.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
