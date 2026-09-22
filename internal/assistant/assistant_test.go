package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigured(t *testing.T) {
	cases := []struct {
		name                   string
		baseURL, apiKey, model string
		want                   bool
	}{
		{"all set", "https://x", "k", "m", true},
		{"missing base", "", "k", "m", false},
		{"missing key", "https://x", "", "m", false},
		{"missing model", "https://x", "k", "", false},
		{"nothing set", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(tc.baseURL, tc.apiKey, tc.model)
			if got := c.Configured(); got != tc.want {
				t.Errorf("Configured() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChatReturnsErrNotConfiguredWithoutAnyNetworkCall(t *testing.T) {
	var hits int
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer double.Close()

	c := New("", "", "", WithHTTPClient(double.Client()))
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Chat() error = %v, want ErrNotConfigured", err)
	}
	if hits != 0 {
		t.Errorf("network calls = %d, want 0 for an unconfigured client", hits)
	}
}

func TestChatPostsSystemPromptModelAndConversation(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"  TASK: x  "}}]}`))
	}))
	defer double.Close()

	c := New(double.URL, "sk-test", "gpt-test", WithHTTPClient(double.Client()))
	reply, err := c.Chat(context.Background(), []Message{
		{Role: "user", Content: "make it sound angrier"},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if reply != "TASK: x" {
		t.Errorf("reply = %q, want the trimmed content", reply)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if gotBody["model"] != "gpt-test" {
		t.Errorf("model = %v, want gpt-test", gotBody["model"])
	}
	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %v, want [system, user]", gotBody["messages"])
	}
	system, ok := messages[0].(map[string]any)
	if !ok || system["role"] != "system" || system["content"] != SystemPrompt {
		t.Errorf("messages[0] = %v, want the fixed system prompt", messages[0])
	}
	user, ok := messages[1].(map[string]any)
	if !ok || user["role"] != "user" || user["content"] != "make it sound angrier" {
		t.Errorf("messages[1] = %v, want the caller's user message", messages[1])
	}
}

func TestChatTrimsTrailingSlashFromBaseURL(t *testing.T) {
	var gotPath string
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer double.Close()

	c := New(double.URL+"/", "k", "m", WithHTTPClient(double.Client()))
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions with no double slash", gotPath)
	}
}

func TestChatSurfacesNonSuccessStatus(t *testing.T) {
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer double.Close()

	c := New(double.URL, "wrong-key", "m", WithHTTPClient(double.Client()))
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("want an error for a 401 upstream response")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error = %q, want it to name the status and body", err.Error())
	}
}

func TestChatRejectsEmptyChoices(t *testing.T) {
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer double.Close()

	c := New(double.URL, "k", "m", WithHTTPClient(double.Client()))
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("want an error when the response carries no choices")
	}
}

func TestChatRejectsEmptyContent(t *testing.T) {
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"   "}}]}`))
	}))
	defer double.Close()

	c := New(double.URL, "k", "m", WithHTTPClient(double.Client()))
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("want an error when the response content is blank")
	}
}
