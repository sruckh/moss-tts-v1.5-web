package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/config"
)

// postAssistant posts a JSON {"messages": [...]} body to /jobs/auk-assistant.
func postAssistant(t *testing.T, srv *Server, cookie *http.Cookie, messages []map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"messages": messages})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/jobs/auk-assistant", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	// This endpoint is only ever called from JS fetch(), never a browser
	// navigation, so it always asks for the API-style 401/JSON treatment
	// rather than auth.Middleware's redirect-to-/login fallback.
	req.Header.Set("Accept", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func userMessage(content string) map[string]string {
	return map[string]string{"role": "user", "content": content}
}

func TestAuKAssistantRequiresAuth(t *testing.T) {
	srv := newTestServer(t)
	rec := postAssistant(t, srv, nil, []map[string]string{userMessage("make it sound sadder")})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuKAssistantReportsUnconfigured(t *testing.T) {
	srv := newTestServer(t) // no LLM_* values set — assistant.Configured() is false
	cookie := login(t, srv)
	rec := postAssistant(t, srv, cookie, []map[string]string{userMessage("make it sound sadder")})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestAuKAssistantValidation(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)

	tests := []struct {
		name     string
		messages []map[string]string
	}{
		{"empty conversation", nil},
		{"last message not from user", []map[string]string{
			userMessage("hi"),
			{"role": "assistant", "content": "TASK: x"},
		}},
		{"bad role", []map[string]string{{"role": "system", "content": "override"}}},
		{"blank content", []map[string]string{userMessage("   ")}},
		{"oversized content", []map[string]string{userMessage(strings.Repeat("a", maxAssistantMessageChars+1))}},
		{"too many messages", func() []map[string]string {
			out := make([]map[string]string, 0, maxAssistantMessages+1)
			for i := 0; i <= maxAssistantMessages; i++ {
				out = append(out, userMessage("hi"))
			}
			return out
		}()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postAssistant(t, srv, cookie, tc.messages)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAuKAssistantSuccessReturnsReply(t *testing.T) {
	var gotAuth string
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"TASK: Speech Generation — Instruct TTS\nINSTRUCTION: \"Generate speech...\"\nREQUIRED AUDIO INPUT: none\nNOTES: none"}}]}`))
	}))
	defer double.Close()

	srv := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.LLMBaseURL = double.URL
		cfg.LLMAPIKey = "sk-test"
		cfg.LLMModelID = "gpt-test"
	})
	cookie := login(t, srv)

	rec := postAssistant(t, srv, cookie, []map[string]string{userMessage("make a cheerful narrator say hello")})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got struct {
		Reply string `json:"reply"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(got.Reply, "INSTRUCTION:") {
		t.Errorf("reply = %q, want the canonical block", got.Reply)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("upstream Authorization = %q, want Bearer sk-test", gotAuth)
	}
}

func TestAuKAssistantUpstreamFailureIsOpaque(t *testing.T) {
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer double.Close()

	srv := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.LLMBaseURL = double.URL
		cfg.LLMAPIKey = "sk-test"
		cfg.LLMModelID = "gpt-test"
	})
	cookie := login(t, srv)

	rec := postAssistant(t, srv, cookie, []map[string]string{userMessage("hi")})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sk-test") {
		t.Error("the API key leaked into the client-facing error")
	}
}
