// Package runpod is the client for the MOSS-TTS-v1.5 serverless endpoint.
//
// Only the background worker imports this. The browser never calls RunPod:
// Cloudflare caps a request at 90s, while a cold start plus inference runs for
// minutes. Submission is therefore async — POST /run returns an id immediately
// and the render is collected later by polling /status/{id}.
//
// Field names are confirmed against handler.py in
// sruckh/mossTTS-v1.5-runpod-serverless: the handler reads
// `reference_audio_base64` and writes it to a temp file whose suffix comes from
// `reference_format` (default "wav"), so a non-WAV reference must declare its
// format or the decoder is handed a mislabelled file.
package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

// RunPod async job statuses.
const (
	StatusInQueue    = "IN_QUEUE"
	StatusInProgress = "IN_PROGRESS"
	StatusCompleted  = "COMPLETED"
	StatusFailed     = "FAILED"
)

// defaultTimeout bounds a single call. /run only enqueues, so it answers fast;
// this is a guard against a hung connection, not a render budget.
const defaultTimeout = 30 * time.Second

// maxErrorBody caps how much of a non-2xx body is kept for the error message,
// which is stored on the job row and shown to the user.
const maxErrorBody = 2 << 10

// Configuration failures. Both are permanent: retrying without an operator
// changing something cannot help, so the worker fails the job immediately
// instead of spinning.
var (
	ErrNoEndpoint = errors.New("runpod: no endpoint configured (set RUNPOD_ENDPOINT)")
	ErrNoAPIKey   = errors.New("runpod: no API key configured (set RUNPOD_API_KEY)")
)

// Error is a non-2xx response from RunPod.
type Error struct {
	StatusCode int
	Body       string
}

func (e *Error) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("runpod: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("runpod: HTTP %d: %s", e.StatusCode, e.Body)
}

// Permanent reports whether retrying this response is pointless. A rejected
// credential or a malformed request will be rejected identically forever; 429
// and 5xx are the transient cases worth another tick.
func (e *Error) Permanent() bool {
	switch e.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// IsPermanent reports whether err is a failure that retrying cannot fix.
func IsPermanent(err error) bool {
	if errors.Is(err, ErrNoEndpoint) || errors.Is(err, ErrNoAPIKey) {
		return true
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Permanent()
	}
	return false
}

// Input is the `input` object of a submission.
//
// It marshals through a map so Extra (a job's stored params_json) can carry
// generation parameters this struct does not name — max_new_tokens and friends
// — without a schema change here. The named fields are written last, so Extra
// can never shadow text, stream or the reference.
type Input struct {
	// Text is the script to render. Required.
	Text string

	// Language is the optional hint; the handler auto-detects when it is empty.
	Language string

	// ReferenceAudioBase64 is the cloned voice's stored reference bytes,
	// base64-encoded. Omitted for stock voices, which have no reference.
	ReferenceAudioBase64 string

	// ReferenceFormat is the reference's container ("wav", "mp3", ...) without
	// the leading dot. The handler uses it as the temp-file suffix.
	ReferenceFormat string

	// Stream stays false: Timbre captures the whole WAV from the completed job
	// rather than reassembling chunks.
	Stream bool

	// Extra is the job's params_json, merged beneath the named fields.
	Extra map[string]any
}

// MarshalJSON renders the input object, named fields winning over Extra.
func (in Input) MarshalJSON() ([]byte, error) {
	payload := make(map[string]any, len(in.Extra)+5)
	maps.Copy(payload, in.Extra)

	payload["text"] = in.Text
	payload["stream"] = in.Stream
	if in.Language != "" {
		payload["language"] = in.Language
	}
	if in.ReferenceAudioBase64 != "" {
		payload["reference_audio_base64"] = in.ReferenceAudioBase64
		if in.ReferenceFormat != "" {
			payload["reference_format"] = in.ReferenceFormat
		}
	}
	return json.Marshal(payload)
}

// Submission is the response to POST /run.
type Submission struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Health is the response to GET /health: worker pool and queue depth.
type Health struct {
	Jobs struct {
		Completed  int `json:"completed"`
		Failed     int `json:"failed"`
		InProgress int `json:"inProgress"`
		InQueue    int `json:"inQueue"`
		Retried    int `json:"retried"`
	} `json:"jobs"`
	Workers struct {
		Idle         int `json:"idle"`
		Initializing int `json:"initializing"`
		Ready        int `json:"ready"`
		Running      int `json:"running"`
		Throttled    int `json:"throttled"`
	} `json:"workers"`
}

// Client talks to one serverless endpoint.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient replaces the underlying HTTP client — the seam the tests use
// to point a Client at an httptest server double.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New builds a client for endpoint (e.g. https://api.runpod.ai/v2/<id>) using
// apiKey as the bearer token. Both may be empty; the resulting client fails
// every call with ErrNoEndpoint / ErrNoAPIKey rather than panicking, so the app
// still boots when Infisical injected nothing and queued jobs fail loudly.
func New(endpoint, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(endpoint, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Configured reports whether both the endpoint and the key are present.
func (c *Client) Configured() bool {
	return c.baseURL != "" && c.apiKey != ""
}

// Submit posts the job to /run and returns the async id RunPod assigns. It does
// not wait for the render.
func (c *Client) Submit(ctx context.Context, in Input) (Submission, error) {
	body, err := json.Marshal(map[string]any{"input": in})
	if err != nil {
		return Submission{}, fmt.Errorf("runpod: encode submission: %w", err)
	}

	var out Submission
	if err := c.do(ctx, http.MethodPost, "/run", body, &out); err != nil {
		return Submission{}, err
	}
	if out.ID == "" {
		return Submission{}, errors.New("runpod: /run returned no job id")
	}
	return out, nil
}

// Health probes the endpoint's worker pool and queue depth.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	if err := c.do(ctx, http.MethodGet, "/health", nil, &out); err != nil {
		return Health{}, err
	}
	return out, nil
}

// do issues one request and decodes a JSON response into out.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	if c.baseURL == "" {
		return ErrNoEndpoint
	}
	if c.apiKey == "" {
		return ErrNoAPIKey
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("runpod: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Never wrap the URL into the message: it is not secret, but the key
		// travels alongside it and this text is persisted on the job row.
		return fmt.Errorf("runpod: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &Error{
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(snippet)),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("runpod: decode %s response: %w", path, err)
	}
	return nil
}
