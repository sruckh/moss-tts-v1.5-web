package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// decodeSubmission reads the {"input": {...}} body a submission posts.
func decodeSubmission(t *testing.T, r *http.Request) map[string]any {
	t.Helper()

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var envelope struct {
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return envelope.Input
}

func TestSubmitPostsRunAndReadsID(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotMethod string
		gotInput  map[string]any
	)
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		gotInput = decodeSubmission(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"abc-123","status":"IN_QUEUE"}`)
	}))
	defer double.Close()

	client := New(double.URL, "test-key", WithHTTPClient(double.Client()))
	got, err := client.Submit(context.Background(), Input{
		Text:                 "hello",
		Language:             "English",
		ReferenceAudioBase64: "QUJD",
		ReferenceFormat:      "mp3",
		Extra:                map[string]any{"max_new_tokens": 512},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if got.ID != "abc-123" {
		t.Errorf("ID = %q, want abc-123", got.ID)
	}
	if got.Status != StatusInQueue {
		t.Errorf("Status = %q, want %s", got.Status, StatusInQueue)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/run" {
		t.Errorf("path = %q, want /run", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}

	// The field names the handler actually reads (handler.py process_reference_audio).
	if gotInput["text"] != "hello" {
		t.Errorf("input.text = %v, want hello", gotInput["text"])
	}
	if gotInput["language"] != "English" {
		t.Errorf("input.language = %v, want English", gotInput["language"])
	}
	if gotInput["reference_audio_base64"] != "QUJD" {
		t.Errorf("input.reference_audio_base64 = %v, want QUJD", gotInput["reference_audio_base64"])
	}
	if gotInput["reference_format"] != "mp3" {
		t.Errorf("input.reference_format = %v, want mp3", gotInput["reference_format"])
	}
	if gotInput["stream"] != false {
		t.Errorf("input.stream = %v, want false", gotInput["stream"])
	}
	if gotInput["max_new_tokens"] != float64(512) {
		t.Errorf("input.max_new_tokens = %v, want 512 (params_json passthrough)", gotInput["max_new_tokens"])
	}
}

func TestSubmitOmitsReferenceForStockVoice(t *testing.T) {
	var gotInput map[string]any
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotInput = decodeSubmission(t, r)
		_, _ = io.WriteString(w, `{"id":"x","status":"IN_QUEUE"}`)
	}))
	defer double.Close()

	client := New(double.URL, "k", WithHTTPClient(double.Client()))
	if _, err := client.Submit(context.Background(), Input{Text: "hi"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if _, ok := gotInput["reference_audio_base64"]; ok {
		t.Error("stock voice submission carried reference_audio_base64")
	}
	if _, ok := gotInput["reference_format"]; ok {
		t.Error("stock voice submission carried reference_format")
	}
	if _, ok := gotInput["language"]; ok {
		t.Error("empty language should be omitted so the handler auto-detects")
	}
}

func TestInputExtraCannotShadowNamedFields(t *testing.T) {
	raw, err := json.Marshal(Input{
		Text:   "real",
		Stream: false,
		Extra:  map[string]any{"text": "spoofed", "stream": true},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got["text"] != "real" {
		t.Errorf("text = %v, want real (Extra must not override)", got["text"])
	}
	if got["stream"] != false {
		t.Errorf("stream = %v, want false (Extra must not override)", got["stream"])
	}
}

func TestSubmitWithoutCredentials(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		key      string
		want     error
	}{
		{"no endpoint", "", "key", ErrNoEndpoint},
		{"no key", "https://api.runpod.ai/v2/x", "", ErrNoAPIKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.endpoint, tc.key).Submit(context.Background(), Input{Text: "hi"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !IsPermanent(err) {
				t.Error("a missing credential must be permanent, or the worker retries forever")
			}
		})
	}
}

func TestSubmitErrorClassification(t *testing.T) {
	tests := []struct {
		status        int
		wantPermanent bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusBadRequest, true},
		{http.StatusNotFound, true},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":"nope"}`)
			}))
			defer double.Close()

			client := New(double.URL, "bad-key", WithHTTPClient(double.Client()))
			_, err := client.Submit(context.Background(), Input{Text: "hi"})
			if err == nil {
				t.Fatal("expected an error")
			}

			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %T (%v), want *runpod.Error", err, err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.status)
			}
			if IsPermanent(err) != tc.wantPermanent {
				t.Errorf("IsPermanent = %v, want %v", IsPermanent(err), tc.wantPermanent)
			}
		})
	}
}

func TestSubmitRejectsResponseWithoutID(t *testing.T) {
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"IN_QUEUE"}`)
	}))
	defer double.Close()

	client := New(double.URL, "k", WithHTTPClient(double.Client()))
	if _, err := client.Submit(context.Background(), Input{Text: "hi"}); err == nil {
		t.Fatal("expected an error when /run returns no id")
	}
}

func TestHealthProbesEndpoint(t *testing.T) {
	var gotPath string
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"jobs":{"inQueue":2},"workers":{"ready":1,"running":3}}`)
	}))
	defer double.Close()

	client := New(double.URL, "k", WithHTTPClient(double.Client()))
	got, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	if gotPath != "/health" {
		t.Errorf("path = %q, want /health", gotPath)
	}
	if got.Jobs.InQueue != 2 {
		t.Errorf("jobs.inQueue = %d, want 2", got.Jobs.InQueue)
	}
	if got.Workers.Running != 3 {
		t.Errorf("workers.running = %d, want 3", got.Workers.Running)
	}
}


func TestStatusQueriesEndpoint(t *testing.T) {
	var (
		gotPath string
		gotAuth string
	)
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"job-99","status":"COMPLETED","delayTime":100,"executionTime":500,"output":{"audio_base64":"QUJD","format":"wav","sample_rate":24000}}`)
	}))
	defer double.Close()

	client := New(double.URL, "k", WithHTTPClient(double.Client()))
	got, err := client.Status(context.Background(), "job-99")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if gotPath != "/status/job-99" {
		t.Errorf("path = %q, want /status/job-99", gotPath)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth = %q, want Bearer k", gotAuth)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want %s", got.Status, StatusCompleted)
	}
	if got.Output.AudioBase64 != "QUJD" {
		t.Errorf("audio_base64 = %q, want QUJD", got.Output.AudioBase64)
	}
	if got.DelayTime != 100 || got.ExecutionTime != 500 {
		t.Errorf("delay/exec = %d/%d, want 100/500", got.DelayTime, got.ExecutionTime)
	}
}

func TestConfigured(t *testing.T) {
	if New("", "").Configured() {
		t.Error("empty client reported configured")
	}
	if !New("https://api.runpod.ai/v2/x/", "k").Configured() {
		t.Error("fully configured client reported unconfigured")
	}
}
