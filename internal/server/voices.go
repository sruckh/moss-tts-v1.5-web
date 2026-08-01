package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/sruckh/timbre/internal/voices"
	"github.com/sruckh/timbre/internal/web"
)

// uploadFormKey is the multipart field holding the reference audio file.
const uploadFormKey = "reference"

// maxUploadBytes caps a reference clip. One-shot clone references are short;
// anything larger is almost certainly the wrong file. The body reader is capped
// slightly above this to leave room for multipart framing.
const maxUploadBytes int64 = 10 * 1024 * 1024

// allowedExt is the allowlist that gates "type" validation by extension. The
// common cloned-voice format is WAV; the rest cover what a browser's audio
// picker may hand over.
var allowedExt = map[string]bool{
	".wav":  true,
	".mp3":  true,
	".flac": true,
	".ogg":  true,
	".m4a":  true,
	".aac":  true,
	".webm": true,
	".opus": true,
}

// handleVoiceLibrary answers GET /voices. An Accept: application/json caller
// (used by the verification checks and any API client) gets the row list; a
// browser gets the rendered voice-library page.
func (s *Server) handleVoiceLibrary(w http.ResponseWriter, r *http.Request) {
	items, err := s.voices.List(r.Context())
	if err != nil {
		serverError(w, r, err)
		return
	}
	if wantsJSON(r) {
		if items == nil {
			items = []voices.Voice{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
		return
	}
	_ = web.VoiceLibrary(items).Render(r.Context(), w)
}

// handleVoiceUpload answers POST /voices/upload (multipart). It validates type
// and size, stores the reference bytes, inserts a voices row (kind=cloned), and
// returns the refreshed grid fragment with the new clone selected.
func (s *Server) handleVoiceUpload(w http.ResponseWriter, r *http.Request) {
	// Cap the wire before parsing: multipart streams the body through this.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20)

	// Buffer 1 MiB in memory; a larger valid upload spills to a temp file.
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			http.Error(w, "reference audio exceeds the 10 MB limit", http.StatusRequestEntityTooLarge)
		default:
			http.Error(w, "could not read upload", http.StatusBadRequest)
		}
		return
	}

	file, header, err := r.FormFile(uploadFormKey)
	if err != nil {
		http.Error(w, "no reference audio provided", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if header.Size > maxUploadBytes {
		http.Error(w, "reference audio exceeds the 10 MB limit", http.StatusRequestEntityTooLarge)
		return
	}

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if !allowedExt[ext] {
		http.Error(w, "unsupported file type; use WAV, MP3, FLAC, or OGG", http.StatusUnsupportedMediaType)
		return
	}

	data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		serverError(w, r, err)
		return
	}
	if int64(len(data)) > maxUploadBytes {
		http.Error(w, "reference audio exceeds the 10 MB limit", http.StatusRequestEntityTooLarge)
		return
	}
	if len(data) == 0 {
		http.Error(w, "reference audio is empty", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = deriveVoiceName(header.Filename)
	}

	id, err := s.voices.CreateCloned(r.Context(), name, ext, data)
	if err != nil {
		serverError(w, r, err)
		return
	}

	items, err := s.voices.List(r.Context())
	if err != nil {
		serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = web.VoiceGrid(items, id).Render(r.Context(), w)
}

// deriveVoiceName turns an uploaded filename into a voice name by stripping its
// directory and extension. Empty or odd filenames fall back to "Cloned voice".
func deriveVoiceName(filename string) string {
	base := filepath.Base(filename)
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	if base = strings.TrimSpace(base); base != "" {
		return base
	}
	return "Cloned voice"
}

// wantsJSON reports whether the caller asked for a JSON response.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// serverError logs an internal error (with the request id) and answers 500 with
// a generic message — never the error text, which could leak internals.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "err", err, "path", r.URL.Path, "req_id", middleware.GetReqID(r.Context()))
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
