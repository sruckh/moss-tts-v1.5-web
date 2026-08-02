// Package jobs owns the render queue: the jobs table, the enqueue path the
// browser drives, and the state transitions the background worker drives.
//
// The two sides are deliberately separate. A browser request only ever inserts
// a `queued` row and returns — it never talks to RunPod, because Cloudflare
// caps a request at 90s while a cold start plus inference runs for minutes.
// Everything that moves a row past `queued` is called from the worker.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The job lifecycle. `queued` is written by the enqueue handler; `submitted`
// and `in_progress` by the submission worker; `ready` and `failed` are
// terminal. These must stay in step with the CHECK constraint in internal/db.
const (
	StatusQueued     = "queued"
	StatusSubmitted  = "submitted"
	StatusInProgress = "in_progress"
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

// MaxTextRunes caps one script. MOSS-TTS renders far less than this in a single
// job; the limit exists so a paste of a whole book cannot occupy the queue.
const MaxTextRunes = 5000

// MaxLanguageLen bounds the optional language hint ("English", "Chinese", ...).
const MaxLanguageLen = 64

// DefaultModel names the model this rack renders with. It is recorded on every
// job at enqueue time so a take stays attributable to what produced it — there
// is one model today, and a WAV rendered now must still say so once there are
// several. This is the single source for that name; the UI reads it from the
// stored job, never from a literal.
const DefaultModel = "MOSS-TTS v1.5"

// Validation failures from Enqueue. The handler maps each to a 400 with the
// error text, so the messages are user-facing.
var (
	ErrEmptyText   = errors.New("enter some text to render")
	ErrTextTooLong = fmt.Errorf("text is longer than %d characters", MaxTextRunes)
	ErrLanguage    = errors.New("language hint is too long")
	ErrNoVoice     = errors.New("pick a voice")
)

// ErrNotFound is returned when no job matches the query.
var ErrNotFound = errors.New("job not found")

// Job is one row of the jobs table, joined to its voice's display name.
type Job struct {
	ID     int64  `json:"id"`
	UserID int64  `json:"user_id"`
	Status string `json:"status"`

	// VoiceID is 0 when the voice was deleted after the job was queued
	// (the FK is ON DELETE SET NULL, so history survives the voice).
	VoiceID   int64  `json:"voice_id"`
	VoiceName string `json:"voice_name"`
	VoiceKind string `json:"voice_kind"`

	Text     string `json:"text"`
	Language string `json:"language,omitempty"`

	// ParamsJSON is the extra generation parameters, verbatim as stored. The
	// worker merges it into the RunPod input object.
	ParamsJSON string `json:"params_json,omitempty"`

	// Model is the model that rendered (or will render) this take, recorded at
	// enqueue time. Empty only for rows written before the column existed and
	// never migrated.
	Model string `json:"model,omitempty"`

	// RunPodID is the async job id returned by POST /run. Its presence is what
	// makes submission idempotent: a row that has one is never submitted again.
	RunPodID string `json:"runpod_id,omitempty"`

	AudioPath  string `json:"audio_path,omitempty"`
	Format     string `json:"format,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
	DelayMS    int64  `json:"delay_ms,omitempty"`
	ExecMS     int64  `json:"exec_ms,omitempty"`

	// Error carries the last failure reason. On a non-terminal status it is a
	// retryable error from an earlier attempt, not a verdict.
	Error    string `json:"error,omitempty"`
	Attempts int    `json:"attempts"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Queued reports whether the job is still waiting for the worker.
func (j Job) Queued() bool { return j.Status == StatusQueued }

// Terminal reports whether the job has finished, either way.
func (j Job) Terminal() bool {
	return j.Status == StatusReady || j.Status == StatusFailed
}

// NewJob is the validated input to Enqueue.
type NewJob struct {
	UserID   int64
	VoiceID  int64
	Text     string
	Language string

	// Model names the model to render with. Empty means DefaultModel, which is
	// what every caller passes today — the rack runs one model.
	Model string

	// Params is optional extra generation parameters (e.g. max_new_tokens).
	// It is stored as JSON and passed through to RunPod at submit time.
	Params map[string]any
}

// StatusForRunPod maps a RunPod async status onto a Timbre job status. RunPod
// answers IN_QUEUE to a fresh submission and, on a warm worker, occasionally
// IN_PROGRESS already; anything unexpected is recorded as submitted, since the
// job demonstrably reached RunPod.
func StatusForRunPod(runpodStatus string) string {
	if strings.EqualFold(runpodStatus, "IN_PROGRESS") {
		return StatusInProgress
	}
	return StatusSubmitted
}

// Store is the jobs data access object.
type Store struct {
	db *sql.DB
}

// NewStore builds a Store over an already-migrated database.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// columns is the shared SELECT list. The LEFT JOIN keeps jobs whose voice was
// deleted; those rows report VoiceID 0 and an empty VoiceName.
const columns = `
	SELECT j.id, j.user_id, j.status,
	       COALESCE(j.voice_id, 0), COALESCE(v.name, ''), COALESCE(v.kind, ''),
	       j.text, COALESCE(j.language, ''), COALESCE(j.params_json, ''),
	       COALESCE(j.model, ''),
	       COALESCE(j.runpod_id, ''),
	       COALESCE(j.audio_path, ''), COALESCE(j.format, ''), COALESCE(j.sample_rate, 0),
	       COALESCE(j.delay_ms, 0), COALESCE(j.exec_ms, 0),
	       COALESCE(j.error, ''), j.attempts,
	       j.created_at, j.updated_at
	FROM jobs j
	LEFT JOIN voices v ON v.id = j.voice_id`

// Enqueue validates the input and inserts a `queued` row. It never contacts
// RunPod — that is the worker's job, and this runs on a browser request.
func (s *Store) Enqueue(ctx context.Context, in NewJob) (int64, error) {
	text := strings.TrimSpace(in.Text)
	switch {
	case text == "":
		return 0, ErrEmptyText
	case utf8.RuneCountInString(text) > MaxTextRunes:
		return 0, ErrTextTooLong
	}

	language := strings.TrimSpace(in.Language)
	if len(language) > MaxLanguageLen {
		return 0, ErrLanguage
	}
	if in.VoiceID <= 0 {
		return 0, ErrNoVoice
	}

	var params sql.Null[string]
	if len(in.Params) > 0 {
		encoded, err := json.Marshal(in.Params)
		if err != nil {
			return 0, fmt.Errorf("encode job params: %w", err)
		}
		params = sql.Null[string]{V: string(encoded), Valid: true}
	}

	var languageValue sql.Null[string]
	if language != "" {
		languageValue = sql.Null[string]{V: language, Valid: true}
	}

	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = DefaultModel
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (user_id, voice_id, text, language, params_json, model, status)
		VALUES (?, ?, ?, ?, ?, ?, 'queued')`,
		in.UserID, in.VoiceID, text, languageValue, params, model)
	if err != nil {
		return 0, fmt.Errorf("enqueue job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("enqueue job id: %w", err)
	}
	return id, nil
}

// ListForUser returns one user's most recent jobs, newest first.
func (s *Store) ListForUser(ctx context.Context, userID int64, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		columns+` WHERE j.user_id = ? ORDER BY j.created_at DESC, j.id DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return scanJobs(rows)
}

// Get returns one job by id.
func (s *Store) Get(ctx context.Context, id int64) (Job, error) {
	rows, err := s.db.QueryContext(ctx, columns+` WHERE j.id = ?`, id)
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	found, err := scanJobs(rows)
	if err != nil {
		return Job{}, err
	}
	if len(found) == 0 {
		return Job{}, ErrNotFound
	}
	return found[0], nil
}

// InFlight counts the jobs already handed to RunPod that have not finished.
// The worker uses it as the budget against the configured max-in-flight.
func (s *Store) InFlight(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE status IN ('submitted', 'in_progress')`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count in-flight jobs: %w", err)
	}
	return count, nil
}

// ClaimQueued returns up to limit jobs that are ready to submit, oldest first.
//
// The `runpod_id IS NULL` predicate is the first half of the idempotency
// guarantee: a row that already reached RunPod is never handed out again, even
// if its status were somehow rolled back. MarkSubmitted is the second half.
func (s *Store) ClaimQueued(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		columns+` WHERE j.status = 'queued' AND j.runpod_id IS NULL
		          ORDER BY j.created_at, j.id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim queued jobs: %w", err)
	}
	return scanJobs(rows)
}

// MarkSubmitted records the RunPod id and advances the status.
//
// The WHERE clause makes this a compare-and-set: it only touches a row that is
// still queued and still has no runpod_id, so a duplicate submission can never
// overwrite the first id. It reports false when the row was already claimed,
// which is the caller's signal that its own submission was a duplicate.
func (s *Store) MarkSubmitted(ctx context.Context, id int64, runpodID, status string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = ?, runpod_id = ?, error = NULL, updated_at = datetime('now')
		WHERE id = ? AND status = 'queued' AND runpod_id IS NULL`,
		status, runpodID, id)
	if err != nil {
		return false, fmt.Errorf("mark job %d submitted: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark job %d submitted: %w", id, err)
	}
	return affected == 1, nil
}

// MarkFailed records a terminal failure. It counts the attempt, so a job that
// failed on its first try still shows attempts=1.
func (s *Store) MarkFailed(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = 'failed', error = ?, attempts = attempts + 1,
		    updated_at = datetime('now')
		WHERE id = ? AND status = 'queued'`, reason, id)
	if err != nil {
		return fmt.Errorf("mark job %d failed: %w", id, err)
	}
	return nil
}

// MarkPollerFailed records a terminal failure during status polling (when status is submitted or in_progress).
func (s *Store) MarkPollerFailed(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = 'failed', error = ?, updated_at = datetime('now')
		WHERE id = ? AND status IN ('submitted', 'in_progress')`, reason, id)
	if err != nil {
		return fmt.Errorf("mark poller job %d failed: %w", id, err)
	}
	return nil
}

// NoteAttempt records a retryable failure: the job stays queued and will be
// picked up on a later tick, but the attempt is counted so the worker can give
// up eventually rather than retrying forever.
func (s *Store) NoteAttempt(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET error = ?, attempts = attempts + 1, updated_at = datetime('now')
		WHERE id = ? AND status = 'queued'`, reason, id)
	if err != nil {
		return fmt.Errorf("note attempt on job %d: %w", id, err)
	}
	return nil
}

// ListPendingRunPod returns up to limit jobs that are in submitted or in_progress status
// and have a RunPod async job ID.
func (s *Store) ListPendingRunPod(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		columns+` WHERE j.status IN ('submitted', 'in_progress') AND j.runpod_id IS NOT NULL AND j.runpod_id != ''
		          ORDER BY j.updated_at, j.id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending runpod jobs: %w", err)
	}
	return scanJobs(rows)
}

// UpdateStatus sets the job's status (e.g. from submitted to in_progress).
func (s *Store) UpdateStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = ?, updated_at = datetime('now')
		WHERE id = ? AND status != 'ready' AND status != 'failed'`, status, id)
	if err != nil {
		return fmt.Errorf("update job %d status: %w", id, err)
	}
	return nil
}

// MarkReady records completed render details and updates status to ready.
func (s *Store) MarkReady(ctx context.Context, id int64, audioPath, format string, sampleRate int, delayMS, execMS int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = 'ready', audio_path = ?, format = ?, sample_rate = ?,
		    delay_ms = ?, exec_ms = ?, error = NULL, updated_at = datetime('now')
		WHERE id = ? AND status != 'ready'`, audioPath, format, sampleRate, delayMS, execMS, id)
	if err != nil {
		return fmt.Errorf("mark job %d ready: %w", id, err)
	}
	return nil
}

// Delete removes a job row by ID and returns the deleted Job (including its AudioPath).
// If userID > 0, it enforces ownership.
func (s *Store) Delete(ctx context.Context, id int64, userID int64) (Job, error) {
	job, err := s.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if userID > 0 && job.UserID != userID {
		return Job{}, ErrNotFound
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ? AND user_id = ?`, id, job.UserID)
	if err != nil {
		return Job{}, fmt.Errorf("delete job %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Job{}, fmt.Errorf("delete job %d: %w", id, err)
	}
	if affected == 0 {
		return Job{}, ErrNotFound
	}
	return job, nil
}

// Params decodes the stored params_json. An empty or malformed value yields a
// nil map rather than an error: bad params must not wedge a job forever.
func (j Job) Params() map[string]any {
	if strings.TrimSpace(j.ParamsJSON) == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(j.ParamsJSON), &out); err != nil {
		return nil
	}
	return out
}

func scanJobs(rows *sql.Rows) ([]Job, error) {
	defer rows.Close()

	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.UserID, &j.Status,
			&j.VoiceID, &j.VoiceName, &j.VoiceKind,
			&j.Text, &j.Language, &j.ParamsJSON,
			&j.Model,
			&j.RunPodID,
			&j.AudioPath, &j.Format, &j.SampleRate,
			&j.DelayMS, &j.ExecMS,
			&j.Error, &j.Attempts,
			&j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
