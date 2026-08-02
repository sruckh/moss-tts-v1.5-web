package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	handle, err := Open(filepath.Join(t.TempDir(), "nested", "timbre.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if err := Migrate(context.Background(), handle); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return handle
}

func TestMigrateCreatesSchema(t *testing.T) {
	handle := openTestDB(t)

	rows, err := handle.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)

	for _, table := range []string{"jobs", "users", "voices"} {
		if !contains(got, table) {
			t.Errorf("missing table %q (have %v)", table, got)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	handle := openTestDB(t)
	if err := Migrate(context.Background(), handle); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestWALEnabled(t *testing.T) {
	handle := openTestDB(t)

	var mode string
	if err := handle.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	// litestream replicates by following the WAL — anything else breaks it.
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestJobStatusIsConstrained(t *testing.T) {
	handle := openTestDB(t)
	ctx := context.Background()

	if _, err := handle.ExecContext(ctx,
		`INSERT INTO users (username, password_hash) VALUES ('admin', 'hash')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO jobs (user_id, text, status) VALUES (1, 'hello', 'nonsense')`); err == nil {
		t.Error("expected the status CHECK constraint to reject 'nonsense'")
	}
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO jobs (user_id, text) VALUES (1, 'hello')`); err != nil {
		t.Errorf("default status insert failed: %v", err)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	handle := openTestDB(t)

	if _, err := handle.Exec(
		`INSERT INTO jobs (user_id, text) VALUES (999, 'orphan')`); err == nil {
		t.Error("expected the jobs.user_id foreign key to reject a missing user")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// jobs.alignment_json arrives with word-level timing: created on fresh DBs by
// the schema and added to existing DBs by Migrate. The player falls back to
// interpolation when it is NULL/empty, so the column itself is the only thing
// the migration must guarantee.
func TestMigrateAddsAlignmentJSON(t *testing.T) {
	handle := openTestDB(t)

	var count int
	if err := handle.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name = 'alignment_json'`).Scan(&count); err != nil {
		t.Fatalf("query column: %v", err)
	}
	if count != 1 {
		t.Errorf("jobs.alignment_json column count = %d, want 1", count)
	}
}
