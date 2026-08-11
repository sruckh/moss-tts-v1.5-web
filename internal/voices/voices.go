// Package voices owns the voice library: the stock default voice seeded at
// startup and one-shot-cloned references uploaded by the user. Reference audio
// is stored as a blob on disk (under the configured audio directory) and read
// back at submit time, where the worker base64-encodes it into the RunPod
// payload. Reference bytes are never served over HTTP.
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
	"slices"
	"strings"
	"unicode/utf8"
)

// KindStock / KindCloned are the two voice provenances.
const (
	KindStock  = "stock"
	KindCloned = "cloned"
)

// stockVoices are seeded into an empty library on startup. There is exactly
// one inference backend — the MOSS-TTS v1.5 RunPod endpoint — and its input
// schema has no model or voice field: with no reference audio the endpoint
// renders its built-in default voice, and voice identity beyond that comes
// only from a cloned reference. So the one stock voice is the MOSS default
// voice; everything else in the library is a user clone.
// LicenseLabel is what the card renders after the model name.
var stockVoices = []struct {
	Name, Model, License string
}{
	{"Moss", "MOSS-TTS v1.5", "OpenMOSS Community"},
}

// ErrNotFound is returned when no voice matches the query.
var ErrNotFound = errors.New("voice not found")

// ErrNoReference is returned when a voice has no stored reference bytes — stock
// voices never have one.
var ErrNoReference = errors.New("voice has no reference audio")

// Rename validation failures. The handler maps each to a 400 with the error
// text, so the messages are user-facing.
var (
	ErrEmptyName    = errors.New("give the voice a name")
	ErrNameTooLong  = fmt.Errorf("voice name is longer than %d characters", MaxNameLen)
	ErrNotRenamable = errors.New("stock voices keep their given name")
)

// MaxNameLen bounds a voice name. A card shows the name on one display line;
// anything longer is a description, not a name.
const MaxNameLen = 60

// Voice is one row of the voices table.
type Voice struct {
	ID                  int64            `json:"id"`
	Kind                string           `json:"kind"`
	Name                string           `json:"name"`
	Model               string           `json:"model"`
	LicenseLabel        string           `json:"license_label"`
	ReferencePath       string           `json:"-"` // volume-relative path; never exposed
	ReferenceTranscript sql.Null[string] `json:"reference_transcript"`
	CreatedAt           string           `json:"created_at"`
	OwnerID             sql.Null[int64]  `json:"owner_id"`
	IsGlobal            bool             `json:"is_global"`
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

// List returns voices accessible to userID through voice_assignments or the
// global flag, oldest first (stock seeded before uploads).
func (s *Store) List(ctx context.Context, userID int64) ([]Voice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT v.id, v.kind, v.name, v.model, v.license_label,
			v.reference_path, v.created_at, v.owner_id, v.is_global, v.reference_transcript
		FROM voices v
		LEFT JOIN voice_assignments va ON va.voice_id = v.id
		WHERE v.is_global = 1 OR va.user_id = ?
		ORDER BY v.created_at, v.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("voices list: %w", err)
	}
	defer rows.Close()

	var out []Voice
	for rows.Next() {
		var (
			v        Voice
			ref      sql.Null[string] // reference_path is NULL for stock voices
			isGlobal int
		)
		if err := rows.Scan(&v.ID, &v.Kind, &v.Name, &v.Model,
			&v.LicenseLabel, &ref, &v.CreatedAt, &v.OwnerID, &isGlobal, &v.ReferenceTranscript); err != nil {
			return nil, fmt.Errorf("voices scan: %w", err)
		}
		v.ReferencePath = ref.V
		v.IsGlobal = isGlobal == 1
		out = append(out, v)
	}
	return out, rows.Err()
}

// CreateCloned stores the reference audio, inserts a kind='cloned' row, and
// assigns the new card to userID. ext includes the leading dot (e.g. ".wav")
// and must already be validated by the caller. The returned id is the new row.
func (s *Store) CreateCloned(ctx context.Context, userID int64, name, ext string, data []byte) (int64, error) {
	rel, err := s.writeReference(ext, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	cleanup := func() { _ = os.Remove(s.absPath(rel)) }

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("begin cloned voice: %w", err)
	}
	defer tx.Rollback()

	var ownerArg any
	if userID > 0 {
		ownerArg = userID
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO voices (kind, name, model, license_label, reference_path, owner_id, is_global)
		VALUES ('cloned', ?, 'Cloned', 'Cloned voice', ?, ?, 0)`, name, rel, ownerArg)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("insert cloned voice: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("cloned voice id: %w", err)
	}
	if userID > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO voice_assignments (voice_id, user_id) VALUES (?, ?)`, id, userID); err != nil {
			cleanup()
			return 0, fmt.Errorf("assign cloned voice: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		cleanup()
		return 0, fmt.Errorf("commit cloned voice: %w", err)
	}
	return id, nil
}

// SeedStock inserts the built-in stock voices that are not already present and
// removes stock rows that are no longer in the seed list. It is idempotent:
// safe on every boot and tolerant of a partially seeded table. The delete is
// what reconciles earlier seeds (whose model names predated the single-model
// MOSS-TTS v1.5 contract) — jobs referencing a removed voice keep their
// history because the FK is ON DELETE SET NULL.
func (s *Store) SeedStock(ctx context.Context) error {
	keep := make([]string, 0, len(stockVoices))
	for _, sv := range stockVoices {
		keep = append(keep, sv.Name)
		var exists int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM voices WHERE kind = 'stock' AND name = ?`, sv.Name).Scan(&exists); err != nil {
			return fmt.Errorf("seed check %q: %w", sv.Name, err)
		}
		if exists > 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO voices (kind, name, model, license_label, is_global)
				VALUES ('stock', ?, ?, ?, 1)`, sv.Name, sv.Model, sv.License); err != nil {
			return fmt.Errorf("seed stock %q: %w", sv.Name, err)
		}
	}

	// Every stock row not in the current seed is stale.
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM voices WHERE kind = 'stock'`)
	if err != nil {
		return fmt.Errorf("seed reconcile list: %w", err)
	}
	var stale []string
	func() {
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return
			}
			if !slices.Contains(keep, name) {
				stale = append(stale, name)
			}
		}
	}()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("seed reconcile list: %w", err)
	}
	for _, name := range stale {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM voices WHERE kind = 'stock' AND name = ?`, name); err != nil {
			return fmt.Errorf("seed reconcile delete %q: %w", name, err)
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

// Get returns one voice by id.
func (s *Store) Get(ctx context.Context, id int64) (Voice, error) {
	var (
		v        Voice
		ref      sql.Null[string]
		isGlobal int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, name, model, license_label, reference_path, created_at, owner_id, is_global, reference_transcript
		FROM voices WHERE id = ?`, id).
		Scan(&v.ID, &v.Kind, &v.Name, &v.Model, &v.LicenseLabel, &ref, &v.CreatedAt, &v.OwnerID, &isGlobal, &v.ReferenceTranscript)
	if errors.Is(err, sql.ErrNoRows) {
		return Voice{}, ErrNotFound
	}
	if err != nil {
		return Voice{}, fmt.Errorf("get voice: %w", err)
	}
	v.ReferencePath = ref.V
	v.IsGlobal = isGlobal == 1
	return v, nil
}

// SetReferenceTranscript updates the reference_transcript for a cloned voice.
func (s *Store) SetReferenceTranscript(ctx context.Context, id int64, transcript string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE voices SET reference_transcript = ? WHERE id = ? AND kind = 'cloned'`, transcript, id)
	if err != nil {
		return fmt.Errorf("set reference transcript for voice %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set reference transcript for voice %d: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearReferenceTranscript sets reference_transcript to NULL for a voice.
func (s *Store) ClearReferenceTranscript(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE voices SET reference_transcript = NULL WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("clear reference transcript for voice %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("clear reference transcript for voice %d: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// IsAccessibleToUser reports whether userID may use voiceID. Global cards are
// available to everyone; private cards require an explicit assignment.
func (s *Store) IsAccessibleToUser(ctx context.Context, voiceID, userID int64) (bool, error) {
	var accessible int
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM voices v
			LEFT JOIN voice_assignments va
				ON va.voice_id = v.id AND va.user_id = ?
			WHERE v.id = ? AND (v.is_global = 1 OR va.user_id IS NOT NULL)
		)`, userID, voiceID).Scan(&accessible); err != nil {
		return false, fmt.Errorf("check voice %d access: %w", voiceID, err)
	}
	return accessible == 1, nil
}

// SetGlobal toggles a voice's is_global flag.
func (s *Store) SetGlobal(ctx context.Context, id int64, isGlobal bool) error {
	var globalVal int
	if isGlobal {
		globalVal = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE voices SET is_global = ? WHERE id = ?`, globalVal, id)
	if err != nil {
		return fmt.Errorf("set global voice %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set global voice %d: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Assign grants userID access through the junction table. owner_id mirrors the
// most recent assignment for legacy schema compatibility; userID <= 0 clears all assignments.
func (s *Store) Assign(ctx context.Context, id int64, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("assign voice %d: %w", id, err)
	}
	defer tx.Rollback()

	if userID <= 0 {
		res, err := tx.ExecContext(ctx, `UPDATE voices SET owner_id = NULL WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("assign voice %d: %w", id, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("assign voice %d: %w", id, err)
		}
		if affected == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM voice_assignments WHERE voice_id = ?`, id); err != nil {
			return fmt.Errorf("unassign voice %d: %w", id, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("assign voice %d: %w", id, err)
		}
		return nil
	}

	res, err := tx.ExecContext(ctx, `UPDATE voices SET owner_id = ? WHERE id = ?`, userID, id)
	if err != nil {
		return fmt.Errorf("assign voice %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("assign voice %d: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO voice_assignments (voice_id, user_id) VALUES (?, ?)`, id, userID); err != nil {
		return fmt.Errorf("assign voice %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("assign voice %d: %w", id, err)
	}
	return nil
}

// Unassign removes one user's access to a voice. It is idempotent: removing a
// missing assignment succeeds as long as the voice itself exists.
func (s *Store) Unassign(ctx context.Context, id int64, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("unassign voice %d: %w", id, err)
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM voices WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("unassign voice %d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM voice_assignments WHERE voice_id = ? AND user_id = ?`, id, userID); err != nil {
		return fmt.Errorf("unassign voice %d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE voices SET owner_id = NULL WHERE id = ? AND owner_id = ?`, id, userID); err != nil {
		return fmt.Errorf("unassign voice %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("unassign voice %d: %w", id, err)
	}
	return nil
}

// Rename sets a cloned voice's display name. The name an upload derives from
// its filename is a starting point, not a verdict — a library is only
// browsable if the user can say what a voice is.
//
// Only clones rename. SeedStock reconciles stock rows *by name*, so a renamed
// stock row would read as stale on the next boot: deleted and reseeded, taking
// every job's voice link with it (the FK is ON DELETE SET NULL). Refusing is
// honest; silently losing history on the next restart is not.
//
// Validation mirrors Enqueue's — user-facing errors, never a silent truncation.
func (s *Store) Rename(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return ErrEmptyName
	case utf8.RuneCountInString(name) > MaxNameLen:
		return ErrNameTooLong
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE voices SET name = ? WHERE id = ? AND kind = 'cloned'`, name, id)
	if err != nil {
		return fmt.Errorf("rename voice %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rename voice %d: %w", id, err)
	}
	if affected == 1 {
		return nil
	}
	// Nothing changed: either the row is gone or it is a stock voice.
	v, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if v.Kind != KindCloned {
		return ErrNotRenamable
	}
	// The row exists and is a clone, so the name was already what was asked for.
	return nil
}

// Reference returns the stored reference audio together with its container
// format ("wav", "mp3", ...), which the RunPod handler needs as
// reference_format: it decodes the base64 into a temp file using the format as
// the filename suffix, so a mislabelled MP3 would reach the loader as a WAV.
// Stock voices have no reference and return ErrNoReference.
func (s *Store) Reference(ctx context.Context, id int64) (data []byte, format string, err error) {
	var ref sql.Null[string]
	err = s.db.QueryRowContext(ctx,
		`SELECT reference_path FROM voices WHERE id = ?`, id).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("load reference path: %w", err)
	}
	if !ref.Valid || ref.V == "" {
		return nil, "", ErrNoReference
	}
	data, err = os.ReadFile(s.absPath(ref.V))
	if err != nil {
		return nil, "", fmt.Errorf("read reference audio: %w", err)
	}
	// "refs/a1b2.wav" -> "wav". The extension was allowlist-validated on upload.
	format = strings.TrimPrefix(filepath.Ext(ref.V), ".")
	if format == "" {
		format = "wav"
	}
	return data, format, nil
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
