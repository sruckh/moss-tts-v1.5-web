package voices

import (
	"context"
	"testing"
)

func TestPendingTranscriptionIDsFiltersOrdersAndLimits(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	userID := deleteTestUser(t, store, "transcription_owner")

	first, err := store.CreateCloned(ctx, userID, "First pending", ".wav", minimalWAV())
	if err != nil {
		t.Fatalf("CreateCloned first: %v", err)
	}
	ready, err := store.CreateCloned(ctx, userID, "Already ready", ".wav", minimalWAV())
	if err != nil {
		t.Fatalf("CreateCloned ready: %v", err)
	}
	last, err := store.CreateCloned(ctx, userID, "Blank pending", ".wav", minimalWAV())
	if err != nil {
		t.Fatalf("CreateCloned last: %v", err)
	}
	if err := store.SetReferenceTranscript(ctx, ready, "Ready transcript."); err != nil {
		t.Fatalf("SetReferenceTranscript ready: %v", err)
	}
	if err := store.SetReferenceTranscript(ctx, last, "   "); err != nil {
		t.Fatalf("SetReferenceTranscript blank: %v", err)
	}
	if err := store.SeedStock(ctx); err != nil {
		t.Fatalf("SeedStock: %v", err)
	}

	ids, err := store.PendingTranscriptionIDs(ctx, 1)
	if err != nil {
		t.Fatalf("PendingTranscriptionIDs limit 1: %v", err)
	}
	if len(ids) != 1 || ids[0] != first {
		t.Fatalf("pending limit 1 = %v, want [%d]", ids, first)
	}

	if err := store.SetReferenceTranscript(ctx, first, "Completed later."); err != nil {
		t.Fatalf("SetReferenceTranscript first: %v", err)
	}
	ids, err = store.PendingTranscriptionIDs(ctx, 10)
	if err != nil {
		t.Fatalf("PendingTranscriptionIDs after completion: %v", err)
	}
	if len(ids) != 1 || ids[0] != last {
		t.Fatalf("pending after completion = %v, want [%d]", ids, last)
	}
}
