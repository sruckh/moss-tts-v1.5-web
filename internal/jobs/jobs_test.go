package jobs

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/db"
)

// newTestStore returns a Store over a migrated scratch database, plus the ids
// of a seeded user and voice to hang jobs off.
func newTestStore(t *testing.T) (*Store, int64, int64) {
	t.Helper()

	handle, err := db.Open(filepath.Join(t.TempDir(), "timbre.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if err := db.Migrate(context.Background(), handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	userID := insertRow(t, handle,
		`INSERT INTO users (username, password_hash) VALUES ('tester', 'x')`)
	voiceID := insertRow(t, handle,
		`INSERT INTO voices (kind, name, model, license_label) VALUES ('stock', 'Moss', 'MOSS-TTS v1.5', 'OpenMOSS Community')`)

	return NewStore(handle), userID, voiceID
}

func insertRow(t *testing.T, handle *sql.DB, query string) int64 {
	t.Helper()

	res, err := handle.Exec(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func TestEnqueueInsertsQueuedRow(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	id, err := store.Enqueue(ctx, NewJob{
		UserID:   userID,
		VoiceID:  voiceID,
		Text:     "  Hello there.  ",
		Language: "English",
		Params:   map[string]any{"max_new_tokens": 512},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusQueued {
		t.Errorf("Status = %q, want %s", got.Status, StatusQueued)
	}
	if got.Text != "Hello there." {
		t.Errorf("Text = %q, want the trimmed script", got.Text)
	}
	if got.RunPodID != "" {
		t.Errorf("RunPodID = %q, want empty on a fresh job", got.RunPodID)
	}
	if got.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0", got.Attempts)
	}
	if got.VoiceName != "Moss" {
		t.Errorf("VoiceName = %q, want Moss (join)", got.VoiceName)
	}
	if params := got.Params(); params["max_new_tokens"] != float64(512) {
		t.Errorf("Params() = %v, want max_new_tokens 512", params)
	}
}

func TestEnqueueValidation(t *testing.T) {
	store, userID, voiceID := newTestStore(t)

	tests := []struct {
		name string
		in   NewJob
		want error
	}{
		{"empty text", NewJob{UserID: userID, VoiceID: voiceID, Text: "   "}, ErrEmptyText},
		{"text too long", NewJob{UserID: userID, VoiceID: voiceID,
			Text: strings.Repeat("a", MaxTextRunes+1)}, ErrTextTooLong},
		{"no voice", NewJob{UserID: userID, Text: "hi"}, ErrNoVoice},
		{"language too long", NewJob{UserID: userID, VoiceID: voiceID, Text: "hi",
			Language: strings.Repeat("x", MaxLanguageLen+1)}, ErrLanguage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.Enqueue(context.Background(), tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMarkSubmittedIsCompareAndSet(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	id, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "hi"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claimed, err := store.MarkSubmitted(ctx, id, "runpod-1", StatusSubmitted)
	if err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if !claimed {
		t.Fatal("first MarkSubmitted did not claim the row")
	}

	// A second id must not overwrite the first — this is the guarantee that
	// makes a double submission harmless.
	claimed, err = store.MarkSubmitted(ctx, id, "runpod-2", StatusSubmitted)
	if err != nil {
		t.Fatalf("second MarkSubmitted: %v", err)
	}
	if claimed {
		t.Error("second MarkSubmitted claimed an already-submitted row")
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RunPodID != "runpod-1" {
		t.Errorf("RunPodID = %q, want runpod-1 (the first id must win)", got.RunPodID)
	}
}

func TestClaimQueuedSkipsSubmittedRows(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	queued, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "first"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	submitted, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "second"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := store.MarkSubmitted(ctx, submitted, "runpod-1", StatusSubmitted); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}

	got, err := store.ClaimQueued(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimQueued: %v", err)
	}
	if len(got) != 1 || got[0].ID != queued {
		t.Fatalf("ClaimQueued returned %d rows (%v), want only job %d", len(got), got, queued)
	}

	inFlight, err := store.InFlight(ctx)
	if err != nil {
		t.Fatalf("InFlight: %v", err)
	}
	if inFlight != 1 {
		t.Errorf("InFlight = %d, want 1", inFlight)
	}
}

func TestClaimQueuedRespectsLimit(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	for range 3 {
		if _, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "hi"}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	got, err := store.ClaimQueued(ctx, 2)
	if err != nil {
		t.Fatalf("ClaimQueued: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ClaimQueued(2) returned %d rows, want 2", len(got))
	}

	none, err := store.ClaimQueued(ctx, 0)
	if err != nil {
		t.Fatalf("ClaimQueued(0): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("ClaimQueued(0) returned %d rows, want 0", len(none))
	}
}

func TestMarkFailedAndNoteAttempt(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	id, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "hi"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := store.NoteAttempt(ctx, id, "connection refused"); err != nil {
		t.Fatalf("NoteAttempt: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusQueued {
		t.Errorf("Status = %q, want the job to stay queued for a retry", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got.Attempts)
	}

	if err := store.MarkFailed(ctx, id, "runpod: HTTP 401"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, err = store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("Status = %q, want %s", got.Status, StatusFailed)
	}
	if got.Error != "runpod: HTTP 401" {
		t.Errorf("Error = %q, want the recorded reason", got.Error)
	}
	if got.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", got.Attempts)
	}
}

func TestMarkFailedLeavesSubmittedJobAlone(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	id, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "hi"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := store.MarkSubmitted(ctx, id, "runpod-1", StatusSubmitted); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if err := store.MarkFailed(ctx, id, "late error"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusSubmitted {
		t.Errorf("Status = %q, want a submitted job to survive a stale failure", got.Status)
	}
}

func TestListForUserIsScopedAndNewestFirst(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	first, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "first"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	second, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "second"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := store.Enqueue(ctx, NewJob{UserID: userID + 999, VoiceID: voiceID, Text: "other"}); err == nil {
		t.Fatal("expected the foreign-key constraint to reject an unknown user")
	}

	got, err := store.ListForUser(ctx, userID, 0)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListForUser returned %d rows, want 2", len(got))
	}
	if got[0].ID != second || got[1].ID != first {
		t.Errorf("order = [%d %d], want newest first [%d %d]",
			got[0].ID, got[1].ID, second, first)
	}
}

func TestGetUnknownJob(t *testing.T) {
	store, _, _ := newTestStore(t)
	if _, err := store.Get(context.Background(), 4242); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStatusForRunPod(t *testing.T) {
	tests := map[string]string{
		"IN_QUEUE":    StatusSubmitted,
		"IN_PROGRESS": StatusInProgress,
		"in_progress": StatusInProgress,
		"":            StatusSubmitted,
		"WEIRD":       StatusSubmitted,
	}
	for in, want := range tests {
		if got := StatusForRunPod(in); got != want {
			t.Errorf("StatusForRunPod(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParamsToleratesGarbage(t *testing.T) {
	if got := (Job{ParamsJSON: "not json"}).Params(); got != nil {
		t.Errorf("Params() = %v, want nil for malformed JSON", got)
	}
	if got := (Job{ParamsJSON: "  "}).Params(); got != nil {
		t.Errorf("Params() = %v, want nil for empty JSON", got)
	}
}


func TestPollerAndDeleteStoreOperations(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	// 1. Enqueue job
	id, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "test poller"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// 2. MarkSubmitted
	if _, err := store.MarkSubmitted(ctx, id, "runpod-100", StatusSubmitted); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}

	// 3. ListPendingRunPod
	pending, err := store.ListPendingRunPod(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingRunPod: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("ListPendingRunPod returned %v, want job %d", pending, id)
	}

	// 4. UpdateStatus to in_progress
	if err := store.UpdateStatus(ctx, id, StatusInProgress); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusInProgress {
		t.Errorf("Status = %q, want %s", got.Status, StatusInProgress)
	}

	// 5. MarkReady
	if err := store.MarkReady(ctx, id, "/data/audio/renders/job_1.wav", "wav", 24000, 100, 500); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	got, err = store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusReady {
		t.Errorf("Status = %q, want ready", got.Status)
	}
	if got.AudioPath != "/data/audio/renders/job_1.wav" {
		t.Errorf("AudioPath = %q, want /data/audio/renders/job_1.wav", got.AudioPath)
	}
	if got.Format != "wav" || got.SampleRate != 24000 || got.DelayMS != 100 || got.ExecMS != 500 {
		t.Errorf("Details = %s/%d/%d/%d, want wav/24000/100/500", got.Format, got.SampleRate, got.DelayMS, got.ExecMS)
	}

	// 6. Delete
	deleted, err := store.Delete(ctx, id, userID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted.ID != id || deleted.AudioPath != "/data/audio/renders/job_1.wav" {
		t.Errorf("deleted = %v, want job %d with AudioPath", deleted, id)
	}

	// 7. Get after Delete returns ErrNotFound
	if _, err := store.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete err = %v, want ErrNotFound", err)
	}
}

func TestMarkPollerFailed(t *testing.T) {
	store, userID, voiceID := newTestStore(t)
	ctx := context.Background()

	id, err := store.Enqueue(ctx, NewJob{UserID: userID, VoiceID: voiceID, Text: "hi"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := store.MarkSubmitted(ctx, id, "runpod-101", StatusSubmitted); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if err := store.MarkPollerFailed(ctx, id, "RunPod error"); err != nil {
		t.Fatalf("MarkPollerFailed: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("Status = %q, want %s", got.Status, StatusFailed)
	}
	if got.Error != "RunPod error" {
		t.Errorf("Error = %q, want 'RunPod error'", got.Error)
	}
}
