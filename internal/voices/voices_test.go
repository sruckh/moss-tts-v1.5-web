package voices

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/sruckh/timbre/internal/db"
)

func newTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	handle, err := db.Open(filepath.Join(t.TempDir(), "timbre.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(context.Background(), handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	audioDir := t.TempDir()
	return NewStore(handle, audioDir), func() { handle.Close() }
}

func TestSeedStockIdempotent(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	countStock := func() int {
		var n int
		if err := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM voices WHERE kind = 'stock'`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	if err := store.SeedStock(ctx); err != nil {
		t.Fatalf("SeedStock: %v", err)
	}
	if got, want := countStock(), len(stockVoices); got != want {
		t.Fatalf("after first seed = %d, want %d", got, want)
	}
	// Re-seed: a no-op, no duplicates.
	if err := store.SeedStock(ctx); err != nil {
		t.Fatalf("SeedStock (second): %v", err)
	}
	if got, want := countStock(), len(stockVoices); got != want {
		t.Fatalf("after second seed = %d, want %d (duplicated)", got, want)
	}

	items, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := map[string]bool{}
	for _, v := range items {
		if v.Kind != KindStock {
			t.Errorf("seeded voice %q is %q, want stock", v.Name, v.Kind)
		}
		if names[v.Name] {
			t.Errorf("duplicate stock voice %q", v.Name)
		}
		names[v.Name] = true
	}
	for _, sv := range stockVoices {
		if !names[sv.Name] {
			t.Errorf("missing stock voice %q", sv.Name)
		}
	}
}

func TestCreateClonedRoundTrip(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	if err := store.SeedStock(ctx); err != nil {
		t.Fatalf("SeedStock: %v", err)
	}

	want := minimalWAV()
	id, err := store.CreateCloned(ctx, "My Clone", ".wav", want)
	if err != nil {
		t.Fatalf("CreateCloned: %v", err)
	}
	if id == 0 {
		t.Fatal("CreateCloned returned id 0")
	}

	// The stored bytes read back exactly — the property Goals 4–5 rely on for
	// inline base64 delivery.
	got, err := store.ReferenceBytes(ctx, id)
	if err != nil {
		t.Fatalf("ReferenceBytes: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(want))
	}

	// base64-encode then decode back to the original audio — the actual path
	// the worker will take when submitting to RunPod.
	enc := base64.StdEncoding.EncodeToString(got)
	if enc == "" {
		t.Fatal("base64 output empty")
	}
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(dec, want) {
		t.Fatal("base64 round-trip did not yield the original bytes")
	}

	// Stock voices have no reference.
	var firstStock int64
	if err := store.db.QueryRowContext(ctx,
		`SELECT id FROM voices WHERE kind = 'stock' LIMIT 1`).Scan(&firstStock); err != nil {
		t.Fatalf("pick stock: %v", err)
	}
	if _, err := store.ReferenceBytes(ctx, firstStock); err != ErrNoReference {
		t.Fatalf("stock ReferenceBytes = %v, want ErrNoReference", err)
	}

	// Cloned voice appears in the list after the stock set.
	items, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != len(stockVoices)+1 {
		t.Fatalf("len(items) = %d, want %d", len(items), len(stockVoices)+1)
	}
	last := items[len(items)-1]
	if last.ID != id || last.Kind != KindCloned || last.Name != "My Clone" {
		t.Errorf("cloned voice = %+v", last)
	}
}

// minimalWAV builds a tiny but valid RIFF/WAVE file so tests exercise the real
// upload path (extension + WAV magic bytes) without binary test fixtures.
func minimalWAV() []byte {
	const sampleRate, bitsPerSample, channels = 8000, 8, 1
	samples := []byte{0x80, 0x90, 0xA0, 0xB0, 0xC0, 0xB0, 0xA0, 0x90}
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+len(samples)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16)) // PCM fmt chunk size
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(bitsPerSample))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(samples)))
	buf.Write(samples)
	return buf.Bytes()
}
