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
	"encoding/base64"
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
	ErrNoEndpoint      = errors.New("runpod: no endpoint configured (set RUNPOD_ENDPOINT)")
	ErrNoHiggsEndpoint = errors.New("runpod: no Higgs endpoint configured (set HIGGS_RUNPOD_ENDPOINT)")
	ErrNoAPIKey        = errors.New("runpod: no API key configured (set RUNPOD_API_KEY)")
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

// HiggsValidationError reports a Higgs payload that the worker's own schema
// would reject before any network call is made (too many references, a
// reference over the decoded size limit, or a missing transcript). Always
// permanent: retrying an oversized or malformed payload cannot succeed.
type HiggsValidationError struct {
	Reason string
}

func (e *HiggsValidationError) Error() string { return "runpod: " + e.Reason }

// IsPermanent reports whether err is a failure that retrying cannot fix.
func IsPermanent(err error) bool {
	if errors.Is(err, ErrNoEndpoint) || errors.Is(err, ErrNoHiggsEndpoint) || errors.Is(err, ErrNoAPIKey) {
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
	var valErr *HiggsValidationError
	if errors.As(err, &valErr) {
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

// HiggsModel is the RunPod model identifier for the Higgs TTS engine
// (bosonai/higgs-tts-3-4b), recorded verbatim in jobs.model.
const HiggsModel = "bosonai/higgs-tts-3-4b"

// Higgs worker limits (sruckh/higgs-tts-3-4b-serverless): at most 4 reference
// clips, 4 MiB decoded audio per reference, 6 MiB decoded total. These match
// the RunPod worker's own server-side cap on references[].audio_base64, so
// SubmitHiggs enforces them itself to fail fast with a clear message rather
// than sending an oversized clip to be rejected by the worker.
const (
	higgsMaxReferences     = 4
	higgsMaxReferenceBytes = 4 << 20 // 4 MiB, decoded (pre-base64) — matches the RunPod worker cap
	higgsMaxTotalBytes     = 6 << 20 // 6 MiB, decoded (pre-base64)
)

// HiggsReference is one cloned-voice reference clip attached to a Higgs
// request. Audio is the raw decoded bytes — SubmitHiggs base64-encodes them,
// the caller never pre-encodes.
type HiggsReference struct {
	Audio  []byte
	Text   string // the clip's transcript (Voice.ReferenceTranscript); required
	Format string // container extension without the dot: "wav", "mp3", "flac", "ogg"
}

// HiggsInput is the `input` object of a Higgs submission.
//
// Speed, Temperature, and TopK left at their zero value resolve to the Higgs
// worker's own defaults (1.0, 0.8, 50) — there is no caller-meaningful use of
// exactly 0 for any of them.
type HiggsInput struct {
	// Text is the script to render. Required. Sent as the engine's "input" key.
	Text string

	// Voice selects a stock Higgs voice. Empty resolves to "default": the
	// engine authoritatively rejects a null/empty voice (ADR: voice: null is
	// forbidden), and reference-only cloning also uses the "default" voice
	// name alongside References.
	Voice string

	// References are the cloned voice's reference clips, at most
	// higgsMaxReferences. SubmitHiggs validates these before ever encoding or
	// sending them.
	References []HiggsReference

	ResponseFormat string  // default "wav"
	Speed          float64 // default 1.0
	Temperature    float64 // default 0.8
	TopK           int     // default 50
}

// MarshalJSON renders the Higgs input object per ADR 002 / the Stage 05
// adapter contract. voice is never omitted and never null.
func (in HiggsInput) MarshalJSON() ([]byte, error) {
	voice := strings.TrimSpace(in.Voice)
	if voice == "" {
		voice = "default"
	}
	responseFormat := in.ResponseFormat
	if responseFormat == "" {
		responseFormat = "wav"
	}
	speed := in.Speed
	if speed == 0 {
		speed = 1.0
	}
	temperature := in.Temperature
	if temperature == 0 {
		temperature = 0.8
	}
	topK := in.TopK
	if topK == 0 {
		topK = 50
	}

	payload := map[string]any{
		"input":           in.Text,
		"model":           HiggsModel,
		"voice":           voice,
		"response_format": responseFormat,
		"speed":           speed,
		"temperature":     temperature,
		"top_k":           topK,
		"stream":          false,
	}
	if len(in.References) > 0 {
		refs := make([]map[string]any, len(in.References))
		for i, ref := range in.References {
			refs[i] = map[string]any{
				"audio_base64": base64.StdEncoding.EncodeToString(ref.Audio),
				"text":         ref.Text,
				"audio_format": ref.Format,
			}
		}
		payload["references"] = refs
	}
	return json.Marshal(payload)
}

// ValidateHiggsReferences enforces the Higgs worker's own reference limits
// (higgsMaxReferences, higgsMaxReferenceBytes, higgsMaxTotalBytes) and that
// every reference carries a transcript, before any bytes are encoded or sent.
func ValidateHiggsReferences(refs []HiggsReference) error {
	if len(refs) > higgsMaxReferences {
		return &HiggsValidationError{
			Reason: fmt.Sprintf("too many references: %d (max %d)", len(refs), higgsMaxReferences),
		}
	}
	var total int
	for i, ref := range refs {
		if len(ref.Audio) > higgsMaxReferenceBytes {
			return &HiggsValidationError{
				Reason: fmt.Sprintf("reference %d is %d bytes decoded (max %d)", i, len(ref.Audio), higgsMaxReferenceBytes),
			}
		}
		if strings.TrimSpace(ref.Text) == "" {
			return &HiggsValidationError{Reason: fmt.Sprintf("reference %d has no transcript text", i)}
		}
		total += len(ref.Audio)
	}
	if total > higgsMaxTotalBytes {
		return &HiggsValidationError{
			Reason: fmt.Sprintf("total reference audio is %d bytes decoded (max %d)", total, higgsMaxTotalBytes),
		}
	}
	return nil
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
	// mossEndpoint and higgsEndpoint are two separately deployed RunPod
	// Serverless endpoints. Requests are routed to one or the other by which
	// method is called (Submit/Status vs SubmitHiggs/StatusHiggs); both share
	// this client's HTTP transport and bearer token.
	mossEndpoint  string
	higgsEndpoint string
	apiKey        string
	http          *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient replaces the underlying HTTP client — the seam the tests use
// to point a Client at an httptest server double.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithHiggsEndpoint sets the second endpoint SubmitHiggs and StatusHiggs
// route to (HIGGS_RUNPOD_ENDPOINT). Left unset, those calls fail with
// ErrNoHiggsEndpoint rather than falling back to the MOSS endpoint.
func WithHiggsEndpoint(endpoint string) Option {
	return func(c *Client) { c.higgsEndpoint = strings.TrimRight(endpoint, "/") }
}

// New builds a client for endpoint (e.g. https://api.runpod.ai/v2/<id>) using
// apiKey as the bearer token. Both may be empty; the resulting client fails
// every call with ErrNoEndpoint / ErrNoAPIKey rather than panicking, so the app
// still boots when Infisical injected nothing and queued jobs fail loudly.
func New(endpoint, apiKey string, opts ...Option) *Client {
	c := &Client{
		mossEndpoint: strings.TrimRight(endpoint, "/"),
		apiKey:       apiKey,
		http:         &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Configured reports whether both the endpoint and the key are present.
func (c *Client) Configured() bool {
	return c.mossEndpoint != "" && c.apiKey != ""
}

// Submit posts the job to /run and returns the async id RunPod assigns. It does
// not wait for the render.
func (c *Client) Submit(ctx context.Context, in Input) (Submission, error) {
	body, err := json.Marshal(map[string]any{"input": in})
	if err != nil {
		return Submission{}, fmt.Errorf("runpod: encode submission: %w", err)
	}

	var out Submission
	if err := c.do(ctx, http.MethodPost, c.mossEndpoint, "/run", body, &out); err != nil {
		return Submission{}, err
	}
	if out.ID == "" {
		return Submission{}, errors.New("runpod: /run returned no job id")
	}
	return out, nil
}

// SubmitHiggs posts a Higgs TTS job to /run on the Higgs endpoint and returns
// the async id RunPod assigns. References are validated against the worker's
// limits (ValidateHiggsReferences) before anything is sent.
func (c *Client) SubmitHiggs(ctx context.Context, in HiggsInput) (Submission, error) {
	if c.higgsEndpoint == "" {
		return Submission{}, ErrNoHiggsEndpoint
	}
	if err := ValidateHiggsReferences(in.References); err != nil {
		return Submission{}, err
	}

	body, err := json.Marshal(map[string]any{"input": in})
	if err != nil {
		return Submission{}, fmt.Errorf("runpod: encode higgs submission: %w", err)
	}

	var out Submission
	if err := c.do(ctx, http.MethodPost, c.higgsEndpoint, "/run", body, &out); err != nil {
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
	if err := c.do(ctx, http.MethodGet, c.mossEndpoint, "/health", nil, &out); err != nil {
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

	// WordTimings is the optional forced-alignment block the serverless worker
	// attaches to non-streaming payloads (absent on streaming, on older workers,
	// or whenever alignment failed). It is a POINTER so a missing key decodes to
	// nil: do() turns a JSON type mismatch into a permanent DecodeError, so a
	// required field here would hard-fail every job from a worker that omits it.
	// Absent key ⇒ nil ⇒ the player falls back to proportional interpolation.
	WordTimings *WordTimings `json:"word_timings,omitempty"`
}

// WordTimings is the optional word-level timing block the serverless worker
// emits from MMS_FA forced alignment. FrameRate and Source are informational;
// Words is the playhead the player walks.
type WordTimings struct {
	FrameRate float64      `json:"frame_rate,omitempty"`
	Source    string       `json:"source,omitempty"`
	Words     []WordTiming `json:"words"`
}

// WordTiming is one spoken word: the model-normalized text actually rendered
// (not the caller's input — the model reflows numbers, punctuation and pinyin)
// and its [Start, End) seconds from the start of the returned WAV.
type WordTiming struct {
	W     string  `json:"w"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
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
	if err := c.do(ctx, http.MethodGet, c.mossEndpoint, "/status/"+id, nil, &out); err != nil {
		return StatusResult{}, err
	}
	return out, nil
}

// StatusHiggs queries GET /status/{id} on the Higgs endpoint. Mirrors Status,
// which stays pinned to the MOSS endpoint — a job's engine determines which
// method the caller uses to poll it.
func (c *Client) StatusHiggs(ctx context.Context, id string) (StatusResult, error) {
	if id == "" {
		return StatusResult{}, errors.New("runpod: empty job id")
	}
	if c.higgsEndpoint == "" {
		return StatusResult{}, ErrNoHiggsEndpoint
	}
	var out StatusResult
	if err := c.do(ctx, http.MethodGet, c.higgsEndpoint, "/status/"+id, nil, &out); err != nil {
		return StatusResult{}, err
	}
	return out, nil
}

// do issues one request and decodes a JSON response into out.
func (c *Client) do(ctx context.Context, method, baseURL, path string, body []byte, out any) error {
	if baseURL == "" {
		return ErrNoEndpoint
	}
	if c.apiKey == "" {
		return ErrNoAPIKey
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
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
