package runpod

import (
	"context"
	"encoding/base64"
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
