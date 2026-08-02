package jobs

import (
	"context"
	"testing"
)

// Every take records what rendered it. The rack runs one model today; the
// column is what keeps a WAV attributable when it runs several.
func TestEnqueueRecordsDefaultModel(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	id, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "Attribute me."})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	job, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.Model != DefaultModel {
		t.Errorf("Model = %q, want %q", job.Model, DefaultModel)
	}
}

// An explicit model is stored verbatim, so adding a second model later needs no
// change here.
func TestEnqueueKeepsExplicitModel(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	id, err := store.Enqueue(ctx, NewJob{
		UserID: userID, VoiceID: voiceID, Text: "A different rack.", Model: "MOSS-TTS v9.9",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	job, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.Model != "MOSS-TTS v9.9" {
		t.Errorf("Model = %q, want MOSS-TTS v9.9", job.Model)
	}
}
