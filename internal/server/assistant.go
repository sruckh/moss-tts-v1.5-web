package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sruckh/timbre/internal/assistant"
)

// Bounds on one assistant turn. This endpoint drives exactly one fixed system
// prompt for AuK — it is not a general-purpose LLM proxy — so the caps exist
// to bound cost and payload size, not to support an open-ended chat product.
const (
	maxAssistantRequestBytes = 32 << 10
	maxAssistantMessages     = 40
	maxAssistantMessageChars = 4000
	assistantTimeout         = 45 * time.Second
)

// handleAuKAssistant answers POST /jobs/auk-assistant: the browser posts the
// running conversation (excluding the system prompt, which Chat always
// prepends server-side) and gets the assistant's next reply. Unlike audio
// rendering this is synchronous — a chat completion finishes in seconds, well
// under Cloudflare's ~90s cap — so there is no queue/poll indirection here.
func (s *Server) handleAuKAssistant(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth.UserID(r); !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAssistantRequestBytes)
	var req struct {
		Messages []assistant.Message `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "could not read the request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateAssistantMessages(req.Messages); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Checked after validation, deliberately: a malformed request is always a
	// 400 regardless of backend configuration, and a well-formed one only
	// then runs into whether the assistant is set up at all.
	if !s.assistant.Configured() {
		http.Error(w, "the AI assistant is not configured", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), assistantTimeout)
	defer cancel()
	reply, err := s.assistant.Chat(ctx, req.Messages)
	if err != nil {
		serverError(w, r, fmt.Errorf("auk assistant: %w", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"reply": reply})
}

// validateAssistantMessages mirrors the conversation shape Chat expects: a
// non-empty, bounded turn history ending on the user's own message, with
// every message attributable to one of the two roles the assistant recognizes.
func validateAssistantMessages(messages []assistant.Message) error {
	if len(messages) == 0 {
		return errors.New("messages must not be empty")
	}
	if len(messages) > maxAssistantMessages {
		return fmt.Errorf("conversation exceeds %d messages", maxAssistantMessages)
	}
	if last := messages[len(messages)-1]; last.Role != "user" {
		return errors.New("the last message must be from the user")
	}
	for _, m := range messages {
		switch m.Role {
		case "user", "assistant":
		default:
			return fmt.Errorf("invalid message role %q", m.Role)
		}
		if strings.TrimSpace(m.Content) == "" {
			return errors.New("message content must not be empty")
		}
		if utf8.RuneCountInString(m.Content) > maxAssistantMessageChars {
			return fmt.Errorf("message exceeds %d characters", maxAssistantMessageChars)
		}
	}
	return nil
}
