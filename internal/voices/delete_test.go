package voices

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
)

func deleteTestUser(t *testing.T, store *Store, username string) int64 {
	t.Helper()
	res, err := store.db.ExecContext(context.Background(),
		`INSERT INTO users (username, password_hash) VALUES (?, 'hash')`, username)
	if err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user %s id: %v", username, err)
	}
	return id
}

func TestDeleteRequiresCreatorAndCleansDatabase(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	creatorID := deleteTestUser(t, store, "creator")
	assigneeID := deleteTestUser(t, store, "assignee")

	voiceID, err := store.CreateCloned(ctx, creatorID, "Disposable", ".wav", minimalWAV())
	if err != nil {
		t.Fatalf("CreateCloned: %v", err)
	}
	if err := store.Assign(ctx, voiceID, assigneeID); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	voice, err := store.Get(ctx, voiceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !voice.CreatorID.Valid || voice.CreatorID.V != creatorID {
		t.Fatalf("creator = %+v, want %d", voice.CreatorID, creatorID)
	}
	if !voice.OwnerID.Valid || voice.OwnerID.V != assigneeID {
		t.Fatalf("owner mirror = %+v, want latest assignee %d", voice.OwnerID, assigneeID)
	}

	jobRes, err := store.db.ExecContext(ctx,
		`INSERT INTO jobs (user_id, voice_id, text) VALUES (?, ?, 'historical')`, creatorID, voiceID)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	jobID, _ := jobRes.LastInsertId()

	if _, err := store.Delete(ctx, voiceID, assigneeID, false); !errors.Is(err, ErrDeleteForbidden) {
		t.Fatalf("assignee delete error = %v, want ErrDeleteForbidden", err)
	}
	if _, err := os.Stat(store.absPath(voice.ReferencePath)); err != nil {
		t.Fatalf("reference disappeared after forbidden delete: %v", err)
	}

	path, err := store.Delete(ctx, voiceID, creatorID, false)
	if err != nil {
		t.Fatalf("creator Delete: %v", err)
	}
	if path != store.absPath(voice.ReferencePath) {
		t.Fatalf("delete path = %q, want %q", path, store.absPath(voice.ReferencePath))
	}
	if _, err := store.Get(ctx, voiceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete error = %v, want ErrNotFound", err)
	}
	var assignments int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM voice_assignments WHERE voice_id = ?`, voiceID).Scan(&assignments); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	if assignments != 0 {
		t.Fatalf("assignments = %d, want 0", assignments)
	}
	var linkedVoice sql.Null[int64]
	if err := store.db.QueryRowContext(ctx,
		`SELECT voice_id FROM jobs WHERE id = ?`, jobID).Scan(&linkedVoice); err != nil {
		t.Fatalf("read historical job: %v", err)
	}
	if linkedVoice.Valid {
		t.Fatalf("historical job voice_id = %d, want NULL", linkedVoice.V)
	}
}

func TestDeleteAdminCanRemoveAnotherCreatorsClone(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	creatorID := deleteTestUser(t, store, "admin_target")

	voiceID, err := store.CreateCloned(ctx, creatorID, "Admin removable", ".wav", minimalWAV())
	if err != nil {
		t.Fatalf("CreateCloned: %v", err)
	}
	if _, err := store.Delete(ctx, voiceID, 999, true); err != nil {
		t.Fatalf("admin Delete: %v", err)
	}
}

func TestDeleteRefusesStockVoice(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	if err := store.SeedStock(ctx); err != nil {
		t.Fatalf("SeedStock: %v", err)
	}
	items, err := store.List(ctx, 0)
	if err != nil || len(items) == 0 {
		t.Fatalf("List stock: items=%v err=%v", items, err)
	}
	if _, err := store.Delete(ctx, items[0].ID, 1, true); !errors.Is(err, ErrNotDeletable) {
		t.Fatalf("stock delete error = %v, want ErrNotDeletable", err)
	}
}
