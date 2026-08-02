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
	"slices"
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

// DecodeError is a response whose shape does not match the client's schema —
// the endpoint changed its payload. Retrying the identical query cannot help,
// so the worker classifies it as permanent and the job fails loudly instead of
// retrying forever.
type DecodeError struct {
	Path string
	Err  error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("runpod: decode %s response: %v", e.Path, e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// IsPermanent reports whether err is a failure that retrying cannot fix.
func IsPermanent(err error) bool {
	if errors.Is(err, ErrNoEndpoint) || errors.Is(err, ErrNoAPIKey) {
		return true
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Permanent()
	}
	var decodeErr *DecodeError
	if errors.As(err, &decodeErr) {
		return true
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


// StatusResult is the response to GET /status/{id}.
type StatusResult struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	DelayTime     int64  `json:"delayTime,omitempty"`
	ExecutionTime int64  `json:"executionTime,omitempty"`
	Output        Output `json:"output,omitempty"`
	Error         any    `json:"error,omitempty"`
}

// Output is the nested output object in a RunPod status response.
type Output struct {
	Status           string `json:"status,omitempty"`
	AudioBase64      string `json:"audio_base64,omitempty"`
	Format           string `json:"format,omitempty"`
	SampleRate       int    `json:"sample_rate,omitempty"`
	DetectedLanguage string `json:"detected_language,omitempty"`
}

// outputJSON is Output without its methods, so the custom unmarshaler can
// delegate to the default decoding without recursing.
type outputJSON Output

// UnmarshalJSON accepts both shapes the endpoint produces. A plain handler
// return lands as an object; since the handler runs with
// return_aggregate_stream (sruckh/mossTTS-v1.5-runpod-serverless), RunPod
// aggregates yields into an ARRAY — the completed payload is its last element
// (or the last one carrying audio). Treating only the object form as valid
// stranded jobs forever: the poll failed to decode and looked transient.
func (o *Output) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	if len(data) > 0 && data[0] == '[' {
		var arr []outputJSON
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		if len(arr) == 0 {
			return nil
		}
		pick := arr[len(arr)-1]
		for i, e := range slices.Backward(arr) {
			if e.AudioBase64 != "" {
				pick = arr[i]
				break
			}
		}
		*o = Output(pick)
		return nil
	}
	var obj outputJSON
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*o = Output(obj)
	return nil
}

// ErrorString formats Error into a string regardless of whether it was returned
// as a string or an object.
func (sr StatusResult) ErrorString() string {
	if sr.Error == nil {
		return ""
	}
	switch v := sr.Error.(type) {
	case string:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// Status queries GET /status/{id} for the progress or completion of an async job.
func (c *Client) Status(ctx context.Context, id string) (StatusResult, error) {
	if id == "" {
		return StatusResult{}, errors.New("runpod: empty job id")
	}
	var out StatusResult
	if err := c.do(ctx, http.MethodGet, "/status/"+id, nil, &out); err != nil {
		return StatusResult{}, err
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
		// A type mismatch means the endpoint's schema changed — permanent.
		// A syntax error or truncated body may be a network glitch — transient.
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) {
			return &DecodeError{Path: path, Err: err}
		}
		return fmt.Errorf("runpod: decode %s response: %w", path, err)
	}
	return nil
}
