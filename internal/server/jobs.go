package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/voices"
	"github.com/sruckh/timbre/internal/web"
)

// queueLimit is how many of a user's jobs the queue fragment shows.
const queueLimit = 50

// maxNewTokensCeiling bounds the one generation parameter the form exposes. The
// handler defaults to 4096; a larger value only buys a longer render.
const maxNewTokensCeiling = 8192

// handleQueuePage renders the compose form and the current queue.
func (s *Server) handleQueuePage(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	items, err := s.jobs.ListForUser(r.Context(), userID, queueLimit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	available, err := s.voices.List(r.Context())
	if err != nil {
		serverError(w, r, err)
		return
	}
	_ = web.QueuePage(items, available).Render(r.Context(), w)
}

// handleQueue answers GET /jobs with the queue fragment (or the row list, for
// an Accept: application/json caller).
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	items, err := s.jobs.ListForUser(r.Context(), userID, queueLimit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	s.renderQueue(w, r, items, 0)
}

// handleCreateJob answers POST /jobs: validate, insert a `queued` row, and
// return the refreshed queue.
//
// It deliberately does not contact RunPod. Submission belongs to the background
// worker — a browser request that waited on RunPod would blow Cloudflare's 90s
// cap on the first cold start.
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}

	voiceID, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("voice_id")), 10, 64)
	if err != nil || voiceID <= 0 {
		http.Error(w, jobs.ErrNoVoice.Error(), http.StatusBadRequest)
		return
	}
	// The voice must exist: a job pinned to a phantom id would only fail later,
	// in the worker, where the user never sees why.
	if _, err := s.voices.Get(r.Context(), voiceID); err != nil {
		if errors.Is(err, voices.ErrNotFound) {
			http.Error(w, "that voice no longer exists", http.StatusBadRequest)
			return
		}
		serverError(w, r, err)
		return
	}

	params, err := parseJobParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := s.jobs.Enqueue(r.Context(), jobs.NewJob{
		UserID:   userID,
		VoiceID:  voiceID,
		Text:     r.PostFormValue("text"),
		Language: r.PostFormValue("language"),
		Params:   params,
	})
	if err != nil {
		// Enqueue's validation errors are written for the user.
		if errors.Is(err, jobs.ErrEmptyText) || errors.Is(err, jobs.ErrTextTooLong) ||
			errors.Is(err, jobs.ErrLanguage) || errors.Is(err, jobs.ErrNoVoice) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		serverError(w, r, err)
		return
	}

	if wantsJSON(r) {
		created, err := s.jobs.Get(r.Context(), id)
		if err != nil {
			serverError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(created)
		return
	}

	items, err := s.jobs.ListForUser(r.Context(), userID, queueLimit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	s.renderQueue(w, r, items, id)
}

// renderQueue writes the queue as JSON or as the HTMX fragment.
func (s *Server) renderQueue(w http.ResponseWriter, r *http.Request, items []jobs.Job, justQueued int64) {
	if wantsJSON(r) {
		if items == nil {
			items = []jobs.Job{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = web.Queue(items, justQueued).Render(r.Context(), w)
}

// parseJobParams reads the optional generation parameters into the map stored
// as params_json and merged into the RunPod input at submit time.
func parseJobParams(r *http.Request) (map[string]any, error) {
	raw := strings.TrimSpace(r.PostFormValue("max_new_tokens"))
	if raw == "" {
		return nil, nil
	}
	tokens, err := strconv.Atoi(raw)
	if err != nil || tokens < 1 || tokens > maxNewTokensCeiling {
		return nil, errors.New("max_new_tokens must be a number between 1 and " +
			strconv.Itoa(maxNewTokensCeiling))
	}
	return map[string]any{"max_new_tokens": tokens}, nil
}


// handleDownloadAudio answers GET /jobs/{id}/audio by streaming the saved WAV file.
func (s *Server) handleDownloadAudio(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}

	job, err := s.jobs.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		serverError(w, r, err)
		return
	}
	if job.UserID != userID {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	if job.Status != jobs.StatusReady || job.AudioPath == "" {
		http.Error(w, "audio not ready", http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(job.AudioPath); err != nil {
		http.Error(w, "audio file missing", http.StatusNotFound)
		return
	}

	ext := job.Format
	if ext == "" {
		ext = "wav"
	}
	contentType := "audio/" + ext
	if ext == "wav" {
		contentType = "audio/wav"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"job-%d.%s\"", job.ID, ext))
	http.ServeFile(w, r, job.AudioPath)
}

// handleDeleteJob answers DELETE /jobs/{id} by removing the job from the DB
// and removing its audio file if present.
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.auth.UserID(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}

	deleted, err := s.jobs.Delete(r.Context(), id, userID)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		serverError(w, r, err)
		return
	}

	if deleted.AudioPath != "" {
		_ = os.Remove(deleted.AudioPath)
	}

	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}

	items, err := s.jobs.ListForUser(r.Context(), userID, queueLimit)
	if err != nil {
		serverError(w, r, err)
		return
	}
	s.renderQueue(w, r, items, 0)
}
