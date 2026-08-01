// Package voices owns the voice library: stock models seeded at startup and
// one-shot-cloned references uploaded by the user. Reference audio is stored as
// a blob on disk (under the configured audio directory) and read back at submit
// time, where the worker base64-encodes it into the RunPod payload. Reference
// bytes are never served over HTTP.
package voices

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// KindStock / KindCloned are the two voice provenances.
const (
	KindStock  = "stock"
	KindCloned = "cloned"
)

// stockVoices are seeded into an empty library on startup: three open-weights
// models so the UI and a render can be exercised without an upload.
// LicenseLabel is the SPDX id; the card renders "model · license".
var stockVoices = []struct {
	Name, Model, License string
}{
	{"Ash", "Chatterbox", "MIT"},
	{"Vellum", "Qwen3-TTS", "Apache-2.0"},
	{"Slate", "Higgs Audio v2", "Apache-2.0"},
}

// ErrNotFound is returned when no voice matches the query.
var ErrNotFound = errors.New("voice not found")

// ErrNoReference is returned when a voice has no stored reference bytes — stock
// voices never have one.
var ErrNoReference = errors.New("voice has no reference audio")

// Voice is one row of the voices table.
type Voice struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Model         string `json:"model"`
	LicenseLabel  string `json:"license_label"`
	ReferencePath string `json:"-"` // volume-relative path; never exposed
	CreatedAt     string `json:"created_at"`
}

// Store is the voice-library data access object. It owns both the voices table
// and the on-disk reference-audio blobs.
type Store struct {
	db       *sql.DB
	audioDir string
}

// NewStore builds a Store. audioDir is where uploaded reference samples are
// written (refs/<id>.<ext>); it should already exist (the app creates it at
// startup).
func NewStore(db *sql.DB, audioDir string) *Store {
	return &Store{db: db, audioDir: audioDir}
}

// List returns every voice, oldest first (stock seeded before uploads).
func (s *Store) List(ctx context.Context) ([]Voice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, name, model, license_label, reference_path, created_at
		FROM voices
		ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("voices list: %w", err)
	}
	defer rows.Close()

	var out []Voice
	for rows.Next() {
		var (
			v   Voice
			ref sql.Null[string] // reference_path is NULL for stock voices
		)
		if err := rows.Scan(&v.ID, &v.Kind, &v.Name, &v.Model,
			&v.LicenseLabel, &ref, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("voices scan: %w", err)
		}
		v.ReferencePath = ref.V
		out = append(out, v)
	}
	return out, rows.Err()
}

// CreateCloned stores the reference audio and inserts a kind='cloned' row.
// ext includes the leading dot (e.g. ".wav") and must already be validated by
// the caller. The returned id is the new row.
func (s *Store) CreateCloned(ctx context.Context, name, ext string, data []byte) (int64, error) {
	rel, err := s.writeReference(ext, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO voices (kind, name, model, license_label, reference_path)
		VALUES ('cloned', ?, 'Cloned', 'Cloned voice', ?)`, name, rel)
	if err != nil {
		// Best effort: remove the orphaned blob so the disk and table agree.
		_ = os.Remove(s.absPath(rel))
		return 0, fmt.Errorf("insert cloned voice: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("cloned voice id: %w", err)
	}
	return id, nil
}

// SeedStock inserts the built-in stock voices that are not already present. It
// is idempotent: safe on every boot and tolerant of a partially seeded table.
func (s *Store) SeedStock(ctx context.Context) error {
	for _, sv := range stockVoices {
		var exists int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM voices WHERE kind = 'stock' AND name = ?`, sv.Name).Scan(&exists); err != nil {
			return fmt.Errorf("seed check %q: %w", sv.Name, err)
		}
		if exists > 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO voices (kind, name, model, license_label)
			VALUES ('stock', ?, ?, ?)`, sv.Name, sv.Model, sv.License); err != nil {
			return fmt.Errorf("seed stock %q: %w", sv.Name, err)
		}
	}
	return nil
}

// ReferenceBytes reads the stored reference audio for a cloned voice, ready to
// be base64-encoded into the RunPod submission payload. Stock voices have no
// reference and return ErrNoReference.
func (s *Store) ReferenceBytes(ctx context.Context, id int64) ([]byte, error) {
	var ref sql.Null[string]
	err := s.db.QueryRowContext(ctx,
		`SELECT reference_path FROM voices WHERE id = ?`, id).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load reference path: %w", err)
	}
	if !ref.Valid || ref.V == "" {
		return nil, ErrNoReference
	}
	data, err := os.ReadFile(s.absPath(ref.V))
	if err != nil {
		return nil, fmt.Errorf("read reference audio: %w", err)
	}
	return data, nil
}

// writeReference writes src to audioDir/refs/<random>.ext and returns the
// volume-relative path ("refs/<random>.ext"). The random name avoids collisions
// and never trusts the uploader's filename.
func (s *Store) writeReference(ext string, src io.Reader) (string, error) {
	dir := filepath.Join(s.audioDir, "refs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create refs dir: %w", err)
	}
	name, err := randomName(ext)
	if err != nil {
		return "", err
	}
	full := filepath.Join(dir, name)
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", fmt.Errorf("create reference file: %w", err)
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		_ = os.Remove(full)
		return "", fmt.Errorf("write reference file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(full)
		return "", fmt.Errorf("close reference file: %w", err)
	}
	return filepath.Join("refs", name), nil
}

// absPath resolves a stored volume-relative reference path against audioDir.
func (s *Store) absPath(rel string) string {
	return filepath.Join(s.audioDir, rel)
}

// randomName returns 16 random bytes hex-encoded plus ext, e.g.
// "a1b2c3d4e5f6a7b8.wav".
func randomName(ext string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate reference name: %w", err)
	}
	return hex.EncodeToString(buf) + ext, nil
}
