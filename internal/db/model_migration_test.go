package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A database created before jobs.model existed must come out of Migrate with
// the column present and its rows attributed — there has only ever been one
// model, so leaving old takes blank would be less accurate, not more.
func TestMigrateAddsAndBackfillsJobsModel(t *testing.T) {
	ctx := context.Background()
	handle, err := Open(filepath.Join(t.TempDir(), "timbre.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer handle.Close()

	// The pre-model schema, as it stood before this migration.
	if _, err := handle.ExecContext(ctx, `
		CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (datetime('now')));
		CREATE TABLE voices (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL
			CHECK (kind IN ('stock','cloned')), name TEXT NOT NULL, model TEXT, license_label TEXT,
			reference_path TEXT, created_at TEXT NOT NULL DEFAULT (datetime('now')));
		CREATE TABLE jobs (id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			voice_id INTEGER REFERENCES voices(id) ON DELETE SET NULL,
			text TEXT NOT NULL, language TEXT, params_json TEXT,
			status TEXT NOT NULL DEFAULT 'queued'
				CHECK (status IN ('queued','submitted','in_progress','ready','failed')),
			runpod_id TEXT, audio_path TEXT, format TEXT, sample_rate INTEGER,
			delay_ms INTEGER, exec_ms INTEGER, error TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')));
		INSERT INTO users (id, username, password_hash) VALUES (1, 'admin', 'x');
		INSERT INTO jobs (user_id, text, status) VALUES (1, 'An older take.', 'ready');`); err != nil {
		t.Fatalf("seed pre-migration schema: %v", err)
	}

	if err := Migrate(ctx, handle); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var model sql.Null[string]
	if err := handle.QueryRowContext(ctx, `SELECT model FROM jobs WHERE id = 1`).Scan(&model); err != nil {
		t.Fatalf("read jobs.model: %v", err)
	}
	if !model.Valid || model.V == "" {
		t.Fatal("the pre-existing take was left with no model")
	}
	if model.V != "MOSS-TTS v1.5" {
		t.Errorf("backfilled model = %q, want MOSS-TTS v1.5", model.V)
	}

	// Still idempotent: a second pass must not fail on the existing column.
	if err := Migrate(ctx, handle); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}
