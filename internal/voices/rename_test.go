package voices

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRenameCloned(t *testing.T) {
	store, closeDB := newTestStore(t)
	defer closeDB()
	ctx := context.Background()

	id, err := store.CreateCloned(ctx, 0, "take-01-final.wav", ".wav", []byte("reference"))
	if err != nil {
		t.Fatalf("CreateCloned: %v", err)
	}

	if err := store.Rename(ctx, id, "  Narrator  "); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	v, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.Name != "Narrator" {
		t.Errorf("Name = %q, want Narrator (trimmed)", v.Name)
	}
}

func TestRenameValidation(t *testing.T) {
	store, closeDB := newTestStore(t)
	defer closeDB()
	ctx := context.Background()

	id, err := store.CreateCloned(ctx, 0, "clip.wav", ".wav", []byte("reference"))
	if err != nil {
		t.Fatalf("CreateCloned: %v", err)
	}

	if err := store.Rename(ctx, id, "   "); !errors.Is(err, ErrEmptyName) {
		t.Errorf("empty name error = %v, want ErrEmptyName", err)
	}
	if err := store.Rename(ctx, id, strings.Repeat("é", MaxNameLen+1)); !errors.Is(err, ErrNameTooLong) {
		t.Errorf("long name error = %v, want ErrNameTooLong", err)
	}
	// Right at the limit, counted in runes rather than bytes.
	if err := store.Rename(ctx, id, strings.Repeat("é", MaxNameLen)); err != nil {
		t.Errorf("name at the limit was rejected: %v", err)
	}
}

// Stock rows are reconciled by name on every boot: renaming one would delete it
// on the next restart and null out every job that used it.
func TestRenameRefusesStock(t *testing.T) {
	store, closeDB := newTestStore(t)
	defer closeDB()
	ctx := context.Background()

	if err := store.SeedStock(ctx); err != nil {
		t.Fatalf("SeedStock: %v", err)
	}
	items, err := store.List(ctx, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("no stock voice seeded")
	}

	if err := store.Rename(ctx, items[0].ID, "Renamed stock"); !errors.Is(err, ErrNotRenamable) {
		t.Fatalf("error = %v, want ErrNotRenamable", err)
	}
	if v, err := store.Get(ctx, items[0].ID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if v.Name == "Renamed stock" {
		t.Error("a stock voice was renamed")
	}
}

func TestRenameUnknownVoice(t *testing.T) {
	store, closeDB := newTestStore(t)
	defer closeDB()

	if err := store.Rename(context.Background(), 4242, "Ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}
