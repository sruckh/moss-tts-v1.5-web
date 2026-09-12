package runpod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The live endpoint runs its handler with return_aggregate_stream, so RunPod
// delivers the output of a completed job as an ARRAY of yields, not an object.
// Decoding must accept both or the poll fails forever (regression: job stuck
// in_progress with "cannot unmarshal array into StatusResult.output").
func TestStatusAcceptsAggregatedArrayOutput(t *testing.T) {
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"job-100","status":"COMPLETED","delayTime":100,"executionTime":500,"output":[{"status":"success","audio_base64":"QUJD","format":"wav","sample_rate":24000,"detected_language":"English"}]}`)
	}))
	defer double.Close()

	client := New(double.URL, "k", WithHTTPClient(double.Client()))
	got, err := client.Status(context.Background(), "job-100")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Output.AudioBase64 != "QUJD" {
		t.Errorf("audio_base64 = %q, want QUJD from the aggregated array", got.Output.AudioBase64)
	}
	if got.Output.Format != "wav" || got.Output.SampleRate != 24000 {
		t.Errorf("format/rate = %q/%d, want wav/24000", got.Output.Format, got.Output.SampleRate)
	}
}

// A response whose shape genuinely does not match the schema (not object, not
// array) means the endpoint changed — that is permanent, never transient.
func TestStatusSchemaMismatchIsPermanent(t *testing.T) {
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"job-101","status":"COMPLETED","output":42}`)
	}))
	defer double.Close()

	client := New(double.URL, "k", WithHTTPClient(double.Client()))
	_, err := client.Status(context.Background(), "job-101")
	if err == nil {
		t.Fatal("Status: expected a decode error for output=42")
	}
	if !IsPermanent(err) {
		t.Errorf("IsPermanent(%v) = false, want true — a schema change must fail the job, not retry forever", err)
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

// decodeHiggsInput reads the {"input": {...}} body a Higgs submission posts.
func decodeHiggsInput(t *testing.T, r *http.Request) map[string]any {
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

// Criterion 3: stock/default voice request formatting, and the voice: null
// prohibition.
func TestHiggsPayloadFormattingStockVoice(t *testing.T) {
	var gotInput map[string]any
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotInput = decodeHiggsInput(t, r)
		_, _ = io.WriteString(w, `{"id":"higgs-1","status":"IN_QUEUE"}`)
	}))
	defer double.Close()

	client := New("", "test-key", WithHiggsEndpoint(double.URL), WithHTTPClient(double.Client()))
	got, err := client.SubmitHiggs(context.Background(), HiggsInput{Text: "The quick brown fox."})
	if err != nil {
		t.Fatalf("SubmitHiggs: %v", err)
	}
	if got.ID != "higgs-1" {
		t.Errorf("ID = %q, want higgs-1", got.ID)
	}

	if gotInput["input"] != "The quick brown fox." {
		t.Errorf("input.input = %v, want the script text", gotInput["input"])
	}
	if gotInput["model"] != HiggsModel {
		t.Errorf("input.model = %v, want %s", gotInput["model"], HiggsModel)
	}
	if v, present := gotInput["voice"]; !present || v != "default" {
		t.Errorf("input.voice = %v (present=%v), want the string default, never null", v, present)
	}
	if gotInput["response_format"] != "wav" {
		t.Errorf("input.response_format = %v, want wav", gotInput["response_format"])
	}
	if gotInput["speed"] != float64(1.0) {
		t.Errorf("input.speed = %v, want 1.0", gotInput["speed"])
	}
	if gotInput["temperature"] != float64(0.8) {
		t.Errorf("input.temperature = %v, want 0.8", gotInput["temperature"])
	}
	if gotInput["top_k"] != float64(50) {
		t.Errorf("input.top_k = %v, want 50", gotInput["top_k"])
	}
	if gotInput["stream"] != false {
		t.Errorf("input.stream = %v, want false", gotInput["stream"])
	}
	if _, present := gotInput["references"]; present {
		t.Error("a request with no references must not send a references key")
	}
}

// Criterion 3: cloned reference request formatting — references[].audio_base64,
// references[].text, references[].audio_format.
func TestHiggsPayloadFormattingClonedReference(t *testing.T) {
	var gotInput map[string]any
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotInput = decodeHiggsInput(t, r)
		_, _ = io.WriteString(w, `{"id":"higgs-2","status":"IN_QUEUE"}`)
	}))
	defer double.Close()

	client := New("", "k", WithHiggsEndpoint(double.URL), WithHTTPClient(double.Client()))
	_, err := client.SubmitHiggs(context.Background(), HiggsInput{
		Text: "hello",
		References: []HiggsReference{
			{Audio: []byte("ABC"), Text: "This is the transcript.", Format: "wav"},
		},
	})
	if err != nil {
		t.Fatalf("SubmitHiggs: %v", err)
	}

	refs, ok := gotInput["references"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("references = %v, want a one-element array", gotInput["references"])
	}
	ref, ok := refs[0].(map[string]any)
	if !ok {
		t.Fatalf("references[0] = %v, want an object", refs[0])
	}
	if ref["audio_base64"] != base64.StdEncoding.EncodeToString([]byte("ABC")) {
		t.Errorf("audio_base64 = %v, want the encoded reference bytes", ref["audio_base64"])
	}
	if ref["text"] != "This is the transcript." {
		t.Errorf("text = %v, want the transcript", ref["text"])
	}
	if ref["audio_format"] != "wav" {
		t.Errorf("audio_format = %v, want wav", ref["audio_format"])
	}
	if gotInput["voice"] != "default" {
		t.Errorf("voice = %v, want default even with a reference clip", gotInput["voice"])
	}
}

// Criterion 4: at most higgsMaxReferences clips.
func TestValidateHiggsReferencesTooMany(t *testing.T) {
	refs := make([]HiggsReference, higgsMaxReferences+1)
	for i := range refs {
		refs[i] = HiggsReference{Audio: []byte("x"), Text: "t", Format: "wav"}
	}
	err := ValidateHiggsReferences(refs)
	if err == nil {
		t.Fatal("want an error for too many references")
	}
	if !IsPermanent(err) {
		t.Error("a reference-count violation must be permanent — retrying cannot fix it")
	}
}

// Criterion 4: 4 MiB decoded per reference.
func TestValidateHiggsReferencesPerReferenceLimit(t *testing.T) {
	refs := []HiggsReference{{Audio: make([]byte, higgsMaxReferenceBytes+1), Text: "t", Format: "wav"}}
	if err := ValidateHiggsReferences(refs); err == nil {
		t.Fatal("want an error for a reference over the per-clip limit")
	}
}

// Criterion 4: 6 MiB decoded total, even when no single reference is over the
// per-clip limit.
func TestValidateHiggsReferencesTotalLimit(t *testing.T) {
	refs := make([]HiggsReference, higgsMaxReferences)
	for i := range refs {
		refs[i] = HiggsReference{Audio: make([]byte, higgsMaxReferenceBytes), Text: "t", Format: "wav"}
	}
	if err := ValidateHiggsReferences(refs); err == nil {
		t.Fatal("want an error: 4 references at the 4 MiB cap total 16 MiB, over the 6 MiB limit")
	}
}

func TestValidateHiggsReferencesRequiresText(t *testing.T) {
	refs := []HiggsReference{{Audio: []byte("x"), Text: "   ", Format: "wav"}}
	if err := ValidateHiggsReferences(refs); err == nil {
		t.Fatal("want an error for a reference without a transcript")
	}
}

func TestValidateHiggsReferencesWithinLimitsPasses(t *testing.T) {
	refs := []HiggsReference{
		{Audio: []byte("x"), Text: "t", Format: "wav"},
		{Audio: []byte("y"), Text: "t", Format: "mp3"},
	}
	if err := ValidateHiggsReferences(refs); err != nil {
		t.Errorf("ValidateHiggsReferences: %v, want nil", err)
	}
}

// A validation failure must never reach the network — an oversized payload
// should not spend a RunPod request just to be rejected upstream.
func TestSubmitHiggsRejectsOversizedPayloadWithoutNetworkCall(t *testing.T) {
	var hits int
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = io.WriteString(w, `{"id":"x","status":"IN_QUEUE"}`)
	}))
	defer double.Close()

	client := New("", "k", WithHiggsEndpoint(double.URL), WithHTTPClient(double.Client()))
	refs := make([]HiggsReference, higgsMaxReferences+1)
	for i := range refs {
		refs[i] = HiggsReference{Audio: []byte("x"), Text: "t", Format: "wav"}
	}
	_, err := client.SubmitHiggs(context.Background(), HiggsInput{Text: "hi", References: refs})
	if err == nil {
		t.Fatal("want a validation error")
	}
	if !IsPermanent(err) {
		t.Error("a validation error must be permanent")
	}
	if hits != 0 {
		t.Errorf("network calls = %d, want 0 — validation must happen before any request", hits)
	}
}

func TestSubmitHiggsWithoutEndpoint(t *testing.T) {
	client := New("https://api.runpod.ai/v2/moss", "key") // no WithHiggsEndpoint
	_, err := client.SubmitHiggs(context.Background(), HiggsInput{Text: "hi"})
	if !errors.Is(err, ErrNoHiggsEndpoint) {
		t.Fatalf("err = %v, want ErrNoHiggsEndpoint", err)
	}
	if !IsPermanent(err) {
		t.Error("a missing Higgs endpoint must be permanent")
	}
}

// Criterion 2: the refactored Client routes MOSS and Higgs requests to their
// own configured endpoints — neither ever reaches the other's server.
func TestMOSSAndHiggsRouteToDistinctEndpoints(t *testing.T) {
	var mossHits, higgsHits int
	var mossAuth, higgsAuth string

	moss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mossHits++
		mossAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"id":"moss-1","status":"IN_QUEUE"}`)
	}))
	defer moss.Close()
	higgs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		higgsHits++
		higgsAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"id":"higgs-1","status":"IN_QUEUE"}`)
	}))
	defer higgs.Close()

	client := New(moss.URL, "shared-key", WithHiggsEndpoint(higgs.URL), WithHTTPClient(moss.Client()))

	if _, err := client.Submit(context.Background(), Input{Text: "hi"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := client.SubmitHiggs(context.Background(), HiggsInput{Text: "hi"}); err != nil {
		t.Fatalf("SubmitHiggs: %v", err)
	}

	if mossHits != 1 {
		t.Errorf("moss endpoint hits = %d, want 1", mossHits)
	}
	if higgsHits != 1 {
		t.Errorf("higgs endpoint hits = %d, want 1 — SubmitHiggs must not hit the MOSS endpoint", higgsHits)
	}
	if mossAuth != "Bearer shared-key" || higgsAuth != "Bearer shared-key" {
		t.Errorf("auth = %q / %q, want both endpoints to share the same bearer token", mossAuth, higgsAuth)
	}
}

func TestStatusHiggsQueriesHiggsEndpoint(t *testing.T) {
	var gotPath string
	higgs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"higgs-9","status":"COMPLETED","output":{"audio_base64":"QUJD","format":"wav"}}`)
	}))
	defer higgs.Close()

	client := New("", "k", WithHiggsEndpoint(higgs.URL), WithHTTPClient(higgs.Client()))
	got, err := client.StatusHiggs(context.Background(), "higgs-9")
	if err != nil {
		t.Fatalf("StatusHiggs: %v", err)
	}
	if gotPath != "/status/higgs-9" {
		t.Errorf("path = %q, want /status/higgs-9", gotPath)
	}
	if got.Output.AudioBase64 != "QUJD" {
		t.Errorf("audio_base64 = %q, want QUJD", got.Output.AudioBase64)
	}
}

func TestStatusHiggsWithoutEndpoint(t *testing.T) {
	client := New("https://api.runpod.ai/v2/moss", "key")
	_, err := client.StatusHiggs(context.Background(), "x")
	if !errors.Is(err, ErrNoHiggsEndpoint) {
		t.Fatalf("err = %v, want ErrNoHiggsEndpoint", err)
	}
}

// word_timings is an optional, additive key the serverless worker attaches.
// It must decode when present and stay nil when absent — a missing key can
// never fail a job, since do() turns a JSON type mismatch into a permanent
// DecodeError (the field is therefore a pointer with omitempty).
func TestOutputDecodesWordTimings(t *testing.T) {
	const withTimings = `{"id":"j","status":"COMPLETED","output":{"status":"success","audio_base64":"QUJD","format":"wav","sample_rate":24000,"word_timings":{"frame_rate":50.0,"source":"mms_fa_forced_alignment","words":[{"w":"Hello,","start":0.02,"end":0.41},{"w":"world.","start":0.45,"end":0.80}]}}}`

	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, withTimings)
	}))
	defer double.Close()

	client := New(double.URL, "k", WithHTTPClient(double.Client()))
	got, err := client.Status(context.Background(), "j")
	if err != nil {
		t.Fatalf("Status with word_timings: %v", err)
	}
	if got.Output.WordTimings == nil {
		t.Fatal("WordTimings = nil, want the decoded block")
	}
	if got.Output.WordTimings.Source != "mms_fa_forced_alignment" {
		t.Errorf("source = %q, want mms_fa_forced_alignment", got.Output.WordTimings.Source)
	}
	if len(got.Output.WordTimings.Words) != 2 {
		t.Fatalf("words = %d, want 2", len(got.Output.WordTimings.Words))
	}
	if w := got.Output.WordTimings.Words[0]; w.W != "Hello," || w.Start != 0.02 || w.End != 0.41 {
		t.Errorf("first word = %+v, want Hello,/0.02/0.41", w)
	}

	// Absent key ⇒ nil, no error (an older worker or a streaming render omits it).
	double2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"j2","status":"COMPLETED","output":{"status":"success","audio_base64":"QUJD","format":"wav","sample_rate":24000}}`)
	}))
	defer double2.Close()

	got2, err := New(double2.URL, "k", WithHTTPClient(double2.Client())).Status(context.Background(), "j2")
	if err != nil {
		t.Fatalf("Status without word_timings: %v", err)
	}
	if got2.Output.WordTimings != nil {
		t.Errorf("WordTimings = %+v, want nil when the key is absent", got2.Output.WordTimings)
	}

	// The aggregated-array form (return_aggregate_stream) must surface
	// word_timings from the same element it takes the audio from.
	double3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"j3","status":"COMPLETED","output":[{"status":"success","audio_base64":"QUJD","format":"wav","sample_rate":24000,"word_timings":{"source":"mms_fa_forced_alignment","words":[{"w":"Hi.","start":0.0,"end":0.3}]}}]}`)
	}))
	defer double3.Close()

	got3, err := New(double3.URL, "k", WithHTTPClient(double3.Client())).Status(context.Background(), "j3")
	if err != nil {
		t.Fatalf("Status array form: %v", err)
	}
	if got3.Output.WordTimings == nil || len(got3.Output.WordTimings.Words) != 1 {
		t.Errorf("array-form WordTimings = %+v, want one word", got3.Output.WordTimings)
	}
}

// --- Breeze TTS 2 -----------------------------------------------------------
//
// Every field name, limit and error code asserted below comes from the Breeze
// worker's own schema_validator.py (sruckh/breezetts-runpod). A payload
// assertion that drifts from those names strands every Breeze job.

// Criterion: clone mode sends reference_audio + reference_text and no instruct.
func TestBreezePayloadCloneMode(t *testing.T) {
	var gotInput map[string]any
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotInput = decodeSubmission(t, r)
		_, _ = io.WriteString(w, `{"id":"breeze-1","status":"IN_QUEUE"}`)
	}))
	defer double.Close()

	client := New("", "test-key", WithBreezeEndpoint(double.URL), WithHTTPClient(double.Client()))
	got, err := client.SubmitBreeze(context.Background(), BreezeInput{
		Text:          "The quick brown fox.",
		Mode:          BreezeModeClone,
		References:    []BreezeReference{{Audio: []byte("ABC")}},
		ReferenceText: "This is the reference transcript.",
	})
	if err != nil {
		t.Fatalf("SubmitBreeze: %v", err)
	}
	if got.ID != "breeze-1" {
		t.Errorf("ID = %q, want breeze-1", got.ID)
	}

	if gotInput["text"] != "The quick brown fox." {
		t.Errorf("input.text = %v, want the script text", gotInput["text"])
	}
	if gotInput["mode"] != BreezeModeClone {
		t.Errorf("input.mode = %v, want %s sent explicitly", gotInput["mode"], BreezeModeClone)
	}
	refs, ok := gotInput["reference_audio"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("input.reference_audio = %v, want a one-element array", gotInput["reference_audio"])
	}
	if refs[0] != base64.StdEncoding.EncodeToString([]byte("ABC")) {
		t.Errorf("reference_audio[0] = %v, want the base64-encoded reference bytes", refs[0])
	}
	if gotInput["reference_text"] != "This is the reference transcript." {
		t.Errorf("input.reference_text = %v, want the reference transcript", gotInput["reference_text"])
	}
	if gotInput["cfg_scale"] != breezeDefaultCfgScale {
		t.Errorf("input.cfg_scale = %v, want the resolved default %v", gotInput["cfg_scale"], breezeDefaultCfgScale)
	}
	if gotInput["response_delivery"] != BreezeDeliveryBase64 {
		t.Errorf("input.response_delivery = %v, want %s pinned on every request", gotInput["response_delivery"], BreezeDeliveryBase64)
	}
	if _, present := gotInput["instruct"]; present {
		t.Error("clone mode must not send instruct — the worker ignores it")
	}
}

// Criterion: direction mode sends reference_audio + reference_text + instruct.
func TestBreezePayloadDirectionMode(t *testing.T) {
	var gotInput map[string]any
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotInput = decodeSubmission(t, r)
		_, _ = io.WriteString(w, `{"id":"breeze-2","status":"IN_QUEUE"}`)
	}))
	defer double.Close()

	client := New("", "k", WithBreezeEndpoint(double.URL), WithHTTPClient(double.Client()))
	_, err := client.SubmitBreeze(context.Background(), BreezeInput{
		Text:          "hello",
		Mode:          BreezeModeDirection,
		References:    []BreezeReference{{Audio: []byte("ABC")}},
		ReferenceText: "transcript",
		Instruct:      "Read it slowly, with warmth.",
		CfgScale:      2.5,
	})
	if err != nil {
		t.Fatalf("SubmitBreeze: %v", err)
	}

	if gotInput["mode"] != BreezeModeDirection {
		t.Errorf("input.mode = %v, want %s", gotInput["mode"], BreezeModeDirection)
	}
	if _, ok := gotInput["reference_audio"].([]any); !ok {
		t.Errorf("input.reference_audio = %v, want the encoded reference array", gotInput["reference_audio"])
	}
	if gotInput["reference_text"] != "transcript" {
		t.Errorf("input.reference_text = %v, want transcript", gotInput["reference_text"])
	}
	if gotInput["instruct"] != "Read it slowly, with warmth." {
		t.Errorf("input.instruct = %v, want the instruction", gotInput["instruct"])
	}
	if gotInput["cfg_scale"] != 2.5 {
		t.Errorf("input.cfg_scale = %v, want the caller's 2.5", gotInput["cfg_scale"])
	}
}

// Criterion: design mode sends instruct only. The worker REJECTS a design
// request carrying reference_audio (forbidden_field_for_mode), so the key must
// be absent — not empty, not null.
func TestBreezePayloadDesignModeCarriesNoReference(t *testing.T) {
	var gotInput map[string]any
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotInput = decodeSubmission(t, r)
		_, _ = io.WriteString(w, `{"id":"breeze-3","status":"IN_QUEUE"}`)
	}))
	defer double.Close()

	client := New("", "k", WithBreezeEndpoint(double.URL), WithHTTPClient(double.Client()))
	_, err := client.SubmitBreeze(context.Background(), BreezeInput{
		Text:     "hello",
		Mode:     BreezeModeDesign,
		Instruct: "A calm, low-pitched narrator.",
	})
	if err != nil {
		t.Fatalf("SubmitBreeze: %v", err)
	}

	if gotInput["mode"] != BreezeModeDesign {
		t.Errorf("input.mode = %v, want %s", gotInput["mode"], BreezeModeDesign)
	}
	if gotInput["instruct"] != "A calm, low-pitched narrator." {
		t.Errorf("input.instruct = %v, want the instruction", gotInput["instruct"])
	}
	if _, present := gotInput["reference_audio"]; present {
		t.Error("design mode sent reference_audio — the worker rejects it as forbidden_field_for_mode")
	}
	if _, present := gotInput["reference_text"]; present {
		t.Error("design mode must not send reference_text")
	}
	if gotInput["response_delivery"] != BreezeDeliveryBase64 {
		t.Errorf("input.response_delivery = %v, want %s", gotInput["response_delivery"], BreezeDeliveryBase64)
	}
}

// The payload shape follows Mode, never which fields happen to be populated:
// stray references on a design input can never reach the worker.
func TestBreezeDesignPayloadIgnoresStrayReferences(t *testing.T) {
	raw, err := json.Marshal(BreezeInput{
		Text:          "hi",
		Mode:          BreezeModeDesign,
		Instruct:      "A bright voice.",
		References:    []BreezeReference{{Audio: []byte("ABC")}},
		ReferenceText: "leftover",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := got["reference_audio"]; present {
		t.Error("a design payload carried reference_audio despite the mode")
	}
	if _, present := got["reference_text"]; present {
		t.Error("a design payload carried reference_text despite the mode")
	}
}

func TestBreezeMarshalRejectsUnknownMode(t *testing.T) {
	_, err := json.Marshal(BreezeInput{Text: "hi", Mode: "whisper"})
	if err == nil {
		t.Fatal("want an error for an unknown mode — the worker answers invalid_mode")
	}
	if !IsPermanent(err) {
		t.Errorf("IsPermanent(%v) = false, want true", err)
	}
}

// Criterion: 4 MiB decoded per clip — accepted at the bound, rejected one byte
// over. The worker checks decoded bytes, so validation happens before encoding.
func TestValidateBreezeReferencesPerClipBoundary(t *testing.T) {
	atBound := []BreezeReference{{Audio: make([]byte, breezeMaxReferenceBytes)}}
	if err := ValidateBreezeReferences(atBound); err != nil {
		t.Errorf("ValidateBreezeReferences at exactly %d bytes = %v, want nil", breezeMaxReferenceBytes, err)
	}

	overBound := []BreezeReference{{Audio: make([]byte, breezeMaxReferenceBytes+1)}}
	err := ValidateBreezeReferences(overBound)
	if err == nil {
		t.Fatalf("want an error one byte over the %d-byte per-clip limit", breezeMaxReferenceBytes)
	}
	if !IsPermanent(err) {
		t.Error("an oversized reference must be permanent — retrying cannot shrink it")
	}
}

// Criterion: 6 MiB decoded total — accepted at the bound, rejected one byte
// over, even when no single clip breaches the per-clip limit.
func TestValidateBreezeReferencesTotalBoundary(t *testing.T) {
	atBound := []BreezeReference{
		{Audio: make([]byte, breezeMaxReferenceBytes)},
		{Audio: make([]byte, breezeMaxTotalBytes-breezeMaxReferenceBytes)},
	}
	if err := ValidateBreezeReferences(atBound); err != nil {
		t.Errorf("ValidateBreezeReferences at exactly %d total bytes = %v, want nil", breezeMaxTotalBytes, err)
	}

	overBound := []BreezeReference{
		{Audio: make([]byte, breezeMaxReferenceBytes)},
		{Audio: make([]byte, breezeMaxTotalBytes-breezeMaxReferenceBytes+1)},
	}
	err := ValidateBreezeReferences(overBound)
	if err == nil {
		t.Fatalf("want an error one byte over the %d-byte total limit", breezeMaxTotalBytes)
	}
	if !IsPermanent(err) {
		t.Error("an oversized reference total must be permanent")
	}
}

// The worker's mode matrix, enforced before a request is spent discovering it.
func TestValidateBreezeInputModeMatrix(t *testing.T) {
	ref := []BreezeReference{{Audio: []byte("ABC")}}

	tests := []struct {
		name    string
		in      BreezeInput
		wantErr bool
	}{
		{"clone complete", BreezeInput{Text: "t", Mode: BreezeModeClone, References: ref, ReferenceText: "x"}, false},
		{"clone without reference", BreezeInput{Text: "t", Mode: BreezeModeClone, ReferenceText: "x"}, true},
		{"clone without reference_text", BreezeInput{Text: "t", Mode: BreezeModeClone, References: ref}, true},
		{"direction complete", BreezeInput{Text: "t", Mode: BreezeModeDirection, References: ref, ReferenceText: "x", Instruct: "i"}, false},
		{"direction without instruct", BreezeInput{Text: "t", Mode: BreezeModeDirection, References: ref, ReferenceText: "x"}, true},
		{"design complete", BreezeInput{Text: "t", Mode: BreezeModeDesign, Instruct: "i"}, false},
		{"design without instruct", BreezeInput{Text: "t", Mode: BreezeModeDesign}, true},
		{"design carrying a reference", BreezeInput{Text: "t", Mode: BreezeModeDesign, Instruct: "i", References: ref}, true},
		{"empty text", BreezeInput{Text: "   ", Mode: BreezeModeDesign, Instruct: "i"}, true},
		{"unknown mode", BreezeInput{Text: "t", Mode: "sing", Instruct: "i"}, true},
		{"empty mode is never inferred", BreezeInput{Text: "t", References: ref, ReferenceText: "x"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBreezeInput(tc.in)
			if tc.wantErr && err == nil {
				t.Fatal("want a validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateBreezeInput: %v, want nil", err)
			}
			if tc.wantErr && !IsPermanent(err) {
				t.Error("a validation error must be permanent")
			}
		})
	}
}

// A payload the worker's own validator would reject must never reach it.
func TestSubmitBreezeRejectsInvalidPayloadWithoutNetworkCall(t *testing.T) {
	var hits int
	double := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = io.WriteString(w, `{"id":"x","status":"IN_QUEUE"}`)
	}))
	defer double.Close()

	client := New("", "k", WithBreezeEndpoint(double.URL), WithHTTPClient(double.Client()))
	_, err := client.SubmitBreeze(context.Background(), BreezeInput{
		Text:       "hi",
		Mode:       BreezeModeDesign,
		Instruct:   "A bright voice.",
		References: []BreezeReference{{Audio: make([]byte, breezeMaxReferenceBytes+1)}},
	})
	if err == nil {
		t.Fatal("want a validation error")
	}
	if !IsPermanent(err) {
		t.Error("a validation error must be permanent")
	}
	if hits != 0 {
		t.Errorf("network calls = %d, want 0 — validation must happen before any request", hits)
	}
}

func TestSubmitBreezeWithoutEndpoint(t *testing.T) {
	client := New("https://api.runpod.ai/v2/moss", "key", WithHiggsEndpoint("https://api.runpod.ai/v2/higgs"))
	_, err := client.SubmitBreeze(context.Background(), BreezeInput{
		Text: "hi", Mode: BreezeModeDesign, Instruct: "i",
	})
	if !errors.Is(err, ErrNoBreezeEndpoint) {
		t.Fatalf("err = %v, want ErrNoBreezeEndpoint", err)
	}
	if !IsPermanent(err) {
		t.Error("a missing Breeze endpoint must be permanent")
	}
}

func TestStatusBreezeWithoutEndpoint(t *testing.T) {
	client := New("https://api.runpod.ai/v2/moss", "key")
	_, err := client.StatusBreeze(context.Background(), "x")
	if !errors.Is(err, ErrNoBreezeEndpoint) {
		t.Fatalf("err = %v, want ErrNoBreezeEndpoint", err)
	}
	if !IsPermanent(err) {
		t.Error("a missing Breeze endpoint must be permanent")
	}
}

// Three engines, three separately deployed endpoints, one bearer token: no
// engine's request ever reaches another engine's server.
func TestBreezeRoutesToItsOwnEndpoint(t *testing.T) {
	var mossHits, higgsHits, breezeHits int
	var breezeAuth, breezePath string

	moss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mossHits++
		_, _ = io.WriteString(w, `{"id":"moss-1","status":"IN_QUEUE"}`)
	}))
	defer moss.Close()
	higgs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		higgsHits++
		_, _ = io.WriteString(w, `{"id":"higgs-1","status":"IN_QUEUE"}`)
	}))
	defer higgs.Close()
	breeze := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		breezeHits++
		breezeAuth, breezePath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = io.WriteString(w, `{"id":"breeze-1","status":"IN_QUEUE"}`)
	}))
	defer breeze.Close()

	client := New(moss.URL, "shared-key",
		WithHiggsEndpoint(higgs.URL),
		WithBreezeEndpoint(breeze.URL),
		WithHTTPClient(moss.Client()))

	if _, err := client.Submit(context.Background(), Input{Text: "hi"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := client.SubmitHiggs(context.Background(), HiggsInput{Text: "hi"}); err != nil {
		t.Fatalf("SubmitHiggs: %v", err)
	}
	if _, err := client.SubmitBreeze(context.Background(), BreezeInput{
		Text: "hi", Mode: BreezeModeDesign, Instruct: "i",
	}); err != nil {
		t.Fatalf("SubmitBreeze: %v", err)
	}

	if mossHits != 1 || higgsHits != 1 || breezeHits != 1 {
		t.Errorf("hits moss/higgs/breeze = %d/%d/%d, want 1/1/1", mossHits, higgsHits, breezeHits)
	}
	if breezePath != "/run" {
		t.Errorf("breeze path = %q, want /run", breezePath)
	}
	if breezeAuth != "Bearer shared-key" {
		t.Errorf("breeze auth = %q, want the shared bearer token", breezeAuth)
	}
}

// The completion Timbre pins for: delivery base64, inline audio_base64, and the
// worker's own 24 kHz sample_rate carried through unconverted.
func TestStatusBreezeReadsBase64Completion(t *testing.T) {
	var gotPath string
	breeze := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"breeze-9","status":"COMPLETED","delayTime":10,"executionTime":20,"output":{"delivery":"base64","audio_base64":"QUJD","size_bytes":3,"mode":"clone","cfg_scale":4.0,"sample_rate":24000,"duration_seconds":1.5}}`)
	}))
	defer breeze.Close()

	client := New("", "k", WithBreezeEndpoint(breeze.URL), WithHTTPClient(breeze.Client()))
	got, err := client.StatusBreeze(context.Background(), "breeze-9")
	if err != nil {
		t.Fatalf("StatusBreeze: %v", err)
	}

	if gotPath != "/status/breeze-9" {
		t.Errorf("path = %q, want /status/breeze-9", gotPath)
	}
	if got.Output.Delivery != BreezeDeliveryBase64 {
		t.Errorf("delivery = %q, want %s — anything else means the pin was ignored", got.Output.Delivery, BreezeDeliveryBase64)
	}
	if got.Output.AudioBase64 != "QUJD" {
		t.Errorf("audio_base64 = %q, want QUJD", got.Output.AudioBase64)
	}
	if got.Output.SampleRate != 24000 {
		t.Errorf("sample_rate = %d, want 24000 carried through unconverted", got.Output.SampleRate)
	}
	if got.Output.SizeBytes != 3 {
		t.Errorf("size_bytes = %d, want 3", got.Output.SizeBytes)
	}
	if got.Output.Mode != BreezeModeClone {
		t.Errorf("mode = %q, want clone echoed back", got.Output.Mode)
	}
	if got.Output.DurationSeconds != 1.5 {
		t.Errorf("duration_seconds = %v, want 1.5", got.Output.DurationSeconds)
	}
	// Breeze sends no word_timings: alignment is the local aligner's job.
	if got.Output.WordTimings != nil {
		t.Errorf("WordTimings = %+v, want nil — Breeze has no native timings", got.Output.WordTimings)
	}
	// Breeze sends no format either; the poller's "wav" default applies.
	if got.Output.Format != "" {
		t.Errorf("format = %q, want empty", got.Output.Format)
	}
	if got.BreezeError() != nil {
		t.Errorf("BreezeError = %v, want nil on a successful completion", got.BreezeError())
	}
}

// A worker that answered s3 despite the pinned response_delivery must be
// visible to the caller, not silently mistaken for missing audio.
func TestStatusBreezeSurfacesS3Delivery(t *testing.T) {
	breeze := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"b","status":"COMPLETED","output":{"delivery":"s3","audio_url":"https://example.invalid/x.wav","size_bytes":9,"sample_rate":24000,"url_expires_in":86400}}`)
	}))
	defer breeze.Close()

	got, err := New("", "k", WithBreezeEndpoint(breeze.URL), WithHTTPClient(breeze.Client())).
		StatusBreeze(context.Background(), "b")
	if err != nil {
		t.Fatalf("StatusBreeze: %v", err)
	}
	if got.Output.Delivery != "s3" {
		t.Errorf("delivery = %q, want s3 so the caller can reject it explicitly", got.Output.Delivery)
	}
	if got.Output.AudioURL == "" {
		t.Error("audio_url was dropped; the caller cannot report what the worker actually returned")
	}
	if got.Output.AudioBase64 != "" {
		t.Error("an s3 delivery carries no inline audio")
	}
}

// The worker's failure envelope, in both places it can land: RunPod's runtime
// lifts a handler-returned "error" key to the top level, but the nested form is
// parsed too rather than being lost.
func TestBreezeErrorEnvelopeParsing(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantCode    string
		wantField   string
		wantMessage string
	}{
		{
			name:        "top-level envelope",
			body:        `{"id":"b1","status":"FAILED","error":{"code":"forbidden_field_for_mode","message":"reference_audio is not allowed in design mode","field":"reference_audio"}}`,
			wantCode:    "forbidden_field_for_mode",
			wantField:   "reference_audio",
			wantMessage: "reference_audio is not allowed in design mode",
		},
		{
			name:        "top-level envelope still wrapped",
			body:        `{"id":"b2","status":"FAILED","error":{"error":{"code":"invalid_mode","message":"mode must be one of clone, design, direction","field":"mode"}}}`,
			wantCode:    "invalid_mode",
			wantField:   "mode",
			wantMessage: "mode must be one of clone, design, direction",
		},
		{
			name:        "nested in output",
			body:        `{"id":"b3","status":"COMPLETED","output":{"error":{"code":"reference_audio_too_large","message":"reference audio exceeds 4194304 bytes","field":"reference_audio"}}}`,
			wantCode:    "reference_audio_too_large",
			wantField:   "reference_audio",
			wantMessage: "reference audio exceeds 4194304 bytes",
		},
		{
			name:        "field omitted when not field-scoped",
			body:        `{"id":"b4","status":"FAILED","error":{"code":"invalid_payload","message":"input must be an object"}}`,
			wantCode:    "invalid_payload",
			wantMessage: "input must be an object",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			breeze := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer breeze.Close()

			got, err := New("", "k", WithBreezeEndpoint(breeze.URL), WithHTTPClient(breeze.Client())).
				StatusBreeze(context.Background(), "b")
			if err != nil {
				t.Fatalf("StatusBreeze: %v", err)
			}
			env := got.BreezeError()
			if env == nil {
				t.Fatal("BreezeError = nil, want the decoded envelope")
			}
			if env.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Code, tc.wantCode)
			}
			if env.Field != tc.wantField {
				t.Errorf("field = %q, want %q", env.Field, tc.wantField)
			}
			if env.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", env.Message, tc.wantMessage)
			}
			// The rendered reason is what lands on the job row: it must name the
			// code and never be empty.
			if reason := env.Error(); !strings.Contains(reason, tc.wantCode) {
				t.Errorf("Error() = %q, want it to name %q", reason, tc.wantCode)
			}
		})
	}
}

// A plain-string RunPod error (a crashed worker, not a Breeze envelope) must
// decode to nil rather than to an empty envelope that hides the real reason.
func TestBreezeErrorIgnoresNonEnvelopeErrors(t *testing.T) {
	breeze := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"b","status":"FAILED","error":"worker exited unexpectedly"}`)
	}))
	defer breeze.Close()

	got, err := New("", "k", WithBreezeEndpoint(breeze.URL), WithHTTPClient(breeze.Client())).
		StatusBreeze(context.Background(), "b")
	if err != nil {
		t.Fatalf("StatusBreeze: %v", err)
	}
	if env := got.BreezeError(); env != nil {
		t.Errorf("BreezeError = %+v, want nil for a non-envelope error", env)
	}
	if got.ErrorString() != "worker exited unexpectedly" {
		t.Errorf("ErrorString = %q, want the raw RunPod reason preserved", got.ErrorString())
	}
}


func validAuKInput(task string) AuKInput {
	return NormalizeAuKInput(AuKInput{
		Task:             task,
		Instruction:      "perform the requested audio task",
		ModelVariant:     AuKVariantFlash,
		NFE:              4,
		ResponseDelivery: AuKDeliveryAuto,
	})
}

func TestValidateAuKInputTaskMatrix(t *testing.T) {
	tests := []struct {
		name    string
		input   AuKInput
		wantErr bool
	}{
		{"instruct", validAuKInput(AuKTaskInstructTTS), false},
		{"instruct forbids source", func() AuKInput { in := validAuKInput(AuKTaskInstructTTS); in.Audio = "YQ=="; return in }(), true},
		{"zero shot", func() AuKInput { in := validAuKInput(AuKTaskZeroShotTTS); in.PromptAudio = "YQ=="; return in }(), false},
		{"zero shot missing prompt", validAuKInput(AuKTaskZeroShotTTS), true},
		{"zero shot forbids source", func() AuKInput { in := validAuKInput(AuKTaskZeroShotTTS); in.PromptAudio = "YQ=="; in.Audio = "YQ=="; return in }(), true},
		{"content edit", func() AuKInput { in := validAuKInput(AuKTaskContentEdit); in.Audio = "YQ=="; return in }(), false},
		{"acoustic edit", func() AuKInput { in := validAuKInput(AuKTaskAcousticEdit); in.Audio = "YQ=="; return in }(), false},
		{"paralinguistic edit", func() AuKInput { in := validAuKInput(AuKTaskParalinguisticEdit); in.Audio = "YQ=="; return in }(), false},
		{"enhancement", func() AuKInput { in := validAuKInput(AuKTaskEnhancement); in.Audio = "YQ=="; return in }(), false},
		{"separation", func() AuKInput { in := validAuKInput(AuKTaskSeparation); in.Audio = "YQ=="; return in }(), false},
		{"edit missing source", validAuKInput(AuKTaskContentEdit), true},
		{"edit forbids prompt", func() AuKInput { in := validAuKInput(AuKTaskContentEdit); in.Audio = "YQ=="; in.PromptAudio = "YQ=="; return in }(), true},
		{"auto instruct", validAuKInput(AuKTaskAuto), false},
		{"auto zero shot", func() AuKInput { in := validAuKInput(AuKTaskAuto); in.PromptAudio = "YQ=="; return in }(), false},
		{"auto rejects bare source", func() AuKInput { in := validAuKInput(AuKTaskAuto); in.Audio = "YQ=="; return in }(), true},
		{"prompt text without prompt", func() AuKInput { in := validAuKInput(AuKTaskInstructTTS); in.PromptText = "hello"; return in }(), true},
		{"missing instruction", func() AuKInput { in := validAuKInput(AuKTaskInstructTTS); in.Instruction = ""; return in }(), true},
		{"invalid task", validAuKInput("weave"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAuKInput(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateAuKInput() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateAuKInputVariantAndDeliveryBounds(t *testing.T) {
	seed := int64(-1)
	tests := []struct {
		name  string
		mutate func(*AuKInput)
	}{
		{"flash nfe low", func(in *AuKInput) { in.NFE = -1 }},
		{"flash nfe high", func(in *AuKInput) { in.NFE = 9 }},
		{"base nfe low", func(in *AuKInput) { in.ModelVariant = AuKVariantBase; in.NFE = 15; in.CfgScale = 2 }},
		{"base nfe high", func(in *AuKInput) { in.ModelVariant = AuKVariantBase; in.NFE = 65; in.CfgScale = 2 }},
		{"base cfg low", func(in *AuKInput) { in.ModelVariant = AuKVariantBase; in.NFE = 32; in.CfgScale = .9 }},
		{"base cfg high", func(in *AuKInput) { in.ModelVariant = AuKVariantBase; in.NFE = 32; in.CfgScale = 5.1 }},
		{"bad variant", func(in *AuKInput) { in.ModelVariant = "turbo" }},
		{"bad delivery", func(in *AuKInput) { in.ResponseDelivery = "mail" }},
		{"negative seed", func(in *AuKInput) { in.Seed = &seed }},
		{"duration low", func(in *AuKInput) { in.GenSeconds = .4 }},
		{"duration high", func(in *AuKInput) { in.GenSeconds = 301 }},
		{"invalid base64", func(in *AuKInput) { in.Task = AuKTaskZeroShotTTS; in.PromptAudio = "%%%" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validAuKInput(AuKTaskInstructTTS)
			tc.mutate(&in)
			if err := ValidateAuKInput(in); err == nil {
				t.Fatal("ValidateAuKInput() succeeded, want error")
			}
		})
	}
	// Flash never rejects a cfg_scale input: the documented worker behavior is
	// "forced to 0.0 for flash", mirrored by NormalizeAuKInput.
	forced := NormalizeAuKInput(AuKInput{Task: AuKTaskInstructTTS, Instruction: "x", CfgScale: 3})
	if forced.CfgScale != 0 {
		t.Errorf("flash cfg_scale = %v, want forced 0", forced.CfgScale)
	}
	for _, nfe := range []int{1, 8} {
		in := validAuKInput(AuKTaskInstructTTS)
		in.NFE = nfe
		if err := ValidateAuKInput(in); err != nil {
			t.Errorf("flash boundary nfe=%d: %v", nfe, err)
		}
	}
	for _, nfe := range []int{16, 64} {
		in := validAuKInput(AuKTaskInstructTTS)
		in.ModelVariant, in.NFE, in.CfgScale = AuKVariantBase, nfe, 2
		if err := ValidateAuKInput(in); err != nil {
			t.Errorf("base boundary nfe=%d: %v", nfe, err)
		}
	}
}

func TestSubmitAuKPayloadAndDistinctEndpoint(t *testing.T) {
	var got map[string]any
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		got = decodeSubmission(t, r)
		_, _ = io.WriteString(w, `{"id":"auk-1","status":"IN_QUEUE"}`)
	}))
	defer server.Close()
	seed := int64(42)
	in := AuKInput{
		Task: AuKTaskZeroShotTTS, Instruction: `Say "hello" with the same voice.`,
		PromptAudio: "YQ==", PromptText: "sample", GenSeconds: 2.5,
		GenText: "hello", ModelVariant: AuKVariantBase, NFE: 32,
		CfgScale: 2.5, Seed: &seed, ResponseDelivery: AuKDeliveryS3,
	}
	client := New("", "shared", WithAuKEndpoint(server.URL), WithHTTPClient(server.Client()))
	submission, err := client.SubmitAuK(context.Background(), in)
	if err != nil {
		t.Fatalf("SubmitAuK: %v", err)
	}
	if submission.ID != "auk-1" || auth != "Bearer shared" {
		t.Fatalf("submission/auth = %+v / %q", submission, auth)
	}
	for key, want := range map[string]any{
		"task": AuKTaskZeroShotTTS, "instruction": in.Instruction,
		"prompt_audio": "YQ==", "prompt_text": "sample",
		"gen_seconds": 2.5, "gen_text": "hello", "model_variant": AuKVariantBase,
		"nfe": float64(32), "cfg_scale": 2.5, "seed": float64(42),
		"response_delivery": AuKDeliveryS3,
	} {
		if got[key] != want {
			t.Errorf("payload[%q] = %#v, want %#v", key, got[key], want)
		}
	}
	if _, ok := got["audio"]; ok {
		t.Error("zero-shot payload unexpectedly contains audio")
	}
}

func TestAuKEndpointStatusHealthAndConfiguration(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/status/") {
			_, _ = io.WriteString(w, `{"id":"auk-1","status":"COMPLETED","output":{"delivery":"s3","audio_url":"https://example.test/a.wav","sample_rate":24000,"model_variant":"flash","nfe":4,"task_executed":"instruct_tts","url_expires_at":"2026-09-13T00:00:00Z"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"jobs":{},"workers":{}}`)
	}))
	defer server.Close()
	client := New("", "key", WithAuKEndpoint(server.URL), WithHTTPClient(server.Client()))
	if !client.AuKConfigured() {
		t.Fatal("AuKConfigured() = false")
	}
	status, err := client.StatusAuK(context.Background(), "auk-1")
	if err != nil {
		t.Fatalf("StatusAuK: %v", err)
	}
	if status.Output.AudioURL == "" || status.Output.TaskExecuted != AuKTaskInstructTTS || status.Output.NFE != 4 {
		t.Fatalf("status output = %+v", status.Output)
	}
	if _, err := client.HealthAuK(context.Background()); err != nil {
		t.Fatalf("HealthAuK: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/status/auk-1" || paths[1] != "/health" {
		t.Fatalf("paths = %v", paths)
	}
	missing := New("", "key")
	if missing.AuKConfigured() {
		t.Fatal("AuKConfigured() = true without endpoint")
	}
	if _, err := missing.SubmitAuK(context.Background(), validAuKInput(AuKTaskInstructTTS)); !errors.Is(err, ErrNoAuKEndpoint) {
		t.Fatalf("SubmitAuK missing endpoint error = %v", err)
	}
	if !IsPermanent(ErrNoAuKEndpoint) {
		t.Fatal("ErrNoAuKEndpoint must be permanent")
	}
}

func TestAuKErrorEnvelopeParsing(t *testing.T) {
	sr := StatusResult{Error: `{"code":"missing_required_field","message":"instruction is required","field":"instruction"}`}
	env := sr.AuKError()
	if env == nil || env.Code != "missing_required_field" || env.Field != "instruction" {
		t.Fatalf("AuKError() = %+v", env)
	}
	if got := env.Error(); !strings.Contains(got, "instruction is required") {
		t.Fatalf("error text = %q", got)
	}
}
