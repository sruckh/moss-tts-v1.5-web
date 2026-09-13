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
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RunPod async job statuses.
const (
	StatusInQueue    = "IN_QUEUE"
	StatusInProgress = "IN_PROGRESS"
	StatusCompleted  = "COMPLETED"
	StatusFailed     = "FAILED"
	StatusCancelled  = "CANCELLED"
	StatusTimedOut   = "TIMED_OUT"
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
	ErrNoEndpoint       = errors.New("runpod: no endpoint configured (set RUNPOD_ENDPOINT)")
	ErrNoHiggsEndpoint  = errors.New("runpod: no Higgs endpoint configured (set HIGGS_RUNPOD_ENDPOINT)")
	ErrNoBreezeEndpoint = errors.New("runpod: no Breeze endpoint configured (set BREEZE_RUNPOD_ENDPOINT)")
	ErrNoAuKEndpoint    = errors.New("runpod: no AuK endpoint configured (set AUK_RUNPOD_ENDPOINT)")
	ErrNoAPIKey         = errors.New("runpod: no API key configured (set RUNPOD_API_KEY)")
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

// BreezeValidationError reports a Breeze payload that the worker's own schema
// validator would reject before any network call is made: an empty script, an
// unknown mode, a mode-matrix violation (a design job carrying a reference, a
// clone job missing its transcript), or a reference over the decoded size
// limit. Always permanent — retrying an identical rejected payload cannot
// succeed.
type BreezeValidationError struct {
	Reason string
}

func (e *BreezeValidationError) Error() string { return "runpod: " + e.Reason }

// IsPermanent reports whether err is a failure that retrying cannot fix.
func IsPermanent(err error) bool {
	if errors.Is(err, ErrNoEndpoint) || errors.Is(err, ErrNoHiggsEndpoint) ||
		errors.Is(err, ErrNoBreezeEndpoint) || errors.Is(err, ErrNoAuKEndpoint) || errors.Is(err, ErrNoAPIKey) {
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
	var breezeValErr *BreezeValidationError
	if errors.As(err, &breezeValErr) {
		return true
	}
	var aukValErr *AuKValidationError
	if errors.As(err, &aukValErr) {
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

// BreezeModel is the RunPod model identifier for the Breeze TTS 2 engine
// (BreezeBlue/Breeze-TTS-2), recorded verbatim in jobs.model.
const BreezeModel = "BreezeBlue/Breeze-TTS-2"

// The three Breeze generation modes (schema_validator.py VALID_MODES). Timbre
// always sends `mode` explicitly and never relies on the worker's inference
// rules: the three modes do not share input requirements, and an inferred mode
// would silently change which fields the worker demands.
const (
	// BreezeModeClone renders the script in a cloned voice: reference audio
	// plus that reference's transcript.
	BreezeModeClone = "clone"

	// BreezeModeDesign renders the script from an instruction alone. The worker
	// REJECTS a design request that carries reference_audio
	// (forbidden_field_for_mode), so a design job carries no reference at all.
	BreezeModeDesign = "design"

	// BreezeModeDirection is clone plus an instruction: reference audio, that
	// reference's transcript, and the instruction.
	BreezeModeDirection = "direction"
)

// BreezeDeliveryBase64 is the only delivery Timbre accepts: it is pinned as
// `response_delivery` on every request and expected back as `delivery` on every
// completion.
//
// The worker's own default is `auto`, which resolves to a presigned B2 URL when
// the deployment has credentials and to inline base64 when it does not — a
// worker-side deployment change would otherwise silently flip Timbre between
// two completion paths. Pinning base64 reuses the poller's existing
// audio_base64 decode path exactly and adds no outbound dependency, no expiry
// race and no second timeout surface. It is deliberately not a field on
// BreezeInput: nothing in Timbre may opt a job out of it.
const BreezeDeliveryBase64 = "base64"

// breezeDefaultCfgScale is the worker's own cfg_scale default. Timbre sends the
// resolved value rather than omitting the key, so the payload always records
// what the render actually used. The worker enforces no range — any float it
// can coerce passes — so a UI range is Timbre's choice, not a worker limit.
const breezeDefaultCfgScale = 4.0

// Breeze worker limits (sruckh/breezetts-runpod schema_validator.py:10-11):
// 4 MiB decoded audio per reference clip, 6 MiB decoded across all clips. The
// worker checks DECODED bytes, not base64 length, so SubmitBreeze validates the
// raw bytes before encoding — failing fast with a clear message instead of
// shipping ~5.6 MiB of base64 to be rejected upstream. There is no clip-count
// cap in the Breeze worker; only these two byte limits.
const (
	breezeMaxReferenceBytes = 4 << 20 // 4 MiB, decoded (pre-base64), per clip
	breezeMaxTotalBytes     = 6 << 20 // 6 MiB, decoded (pre-base64), all clips
)

// BreezeReference is one reference clip attached to a clone or direction
// request. Audio is the raw decoded bytes — SubmitBreeze base64-encodes them,
// the caller never pre-encodes.
//
// Unlike HiggsReference there is no per-clip text or format: the Breeze worker
// takes one top-level reference_text for the whole request and infers the
// container itself.
type BreezeReference struct {
	Audio []byte
}

// BreezeInput is the `input` object of a Breeze submission.
//
// Which fields are sent is a function of Mode alone — see MarshalJSON. The
// struct carries no ResponseDelivery field on purpose (see
// breezeResponseDelivery).
type BreezeInput struct {
	// Text is the script to render. Required in every mode, non-empty.
	// Inline vocal events — (laugh) / [笑] — travel here untouched; Timbre
	// never rewrites user text.
	Text string

	// Mode is one of BreezeModeClone, BreezeModeDesign, BreezeModeDirection.
	// Always sent explicitly; an empty or unknown value is a caller bug and is
	// rejected rather than inferred.
	Mode string

	// References are the reference clips for clone and direction. Forbidden in
	// design mode — the worker rejects the request outright.
	References []BreezeReference

	// ReferenceText is the transcript of the reference audio, sent as the
	// worker's top-level `reference_text`. Required for clone and direction.
	ReferenceText string

	// Instruct is the natural-language voice instruction. Required for design
	// and direction; ignored by the worker in clone mode and therefore not sent
	// there.
	Instruct string

	// CfgScale is the guidance scale. Zero resolves to breezeDefaultCfgScale —
	// there is no caller-meaningful use of exactly 0.
	CfgScale float64
}

// MarshalJSON renders the Breeze input object per the mode matrix
// (schema_validator.py:120). The payload shape is derived from Mode rather than
// from which fields happen to be populated, so a design request can never carry
// reference_audio even if a caller left references attached.
func (in BreezeInput) MarshalJSON() ([]byte, error) {
	cfgScale := in.CfgScale
	if cfgScale == 0 {
		cfgScale = breezeDefaultCfgScale
	}

	payload := map[string]any{
		"text":              in.Text,
		"mode":              in.Mode,
		"cfg_scale":         cfgScale,
		"response_delivery": BreezeDeliveryBase64,
	}
	switch in.Mode {
	case BreezeModeClone:
		payload["reference_audio"] = encodeBreezeReferences(in.References)
		payload["reference_text"] = in.ReferenceText
	case BreezeModeDirection:
		payload["reference_audio"] = encodeBreezeReferences(in.References)
		payload["reference_text"] = in.ReferenceText
		payload["instruct"] = in.Instruct
	case BreezeModeDesign:
		payload["instruct"] = in.Instruct
	default:
		return nil, &BreezeValidationError{Reason: breezeUnknownModeReason(in.Mode)}
	}
	return json.Marshal(payload)
}

// encodeBreezeReferences base64-encodes each clip. The worker accepts a bare
// string or an array; Timbre always sends the array form, which is the same
// shape for one clip and for many and is the form the total-bytes limit is
// written against.
func encodeBreezeReferences(refs []BreezeReference) []string {
	encoded := make([]string, len(refs))
	for i, ref := range refs {
		encoded[i] = base64.StdEncoding.EncodeToString(ref.Audio)
	}
	return encoded
}

func breezeUnknownModeReason(mode string) string {
	return fmt.Sprintf("unknown breeze mode %q (want %s, %s or %s)",
		mode, BreezeModeClone, BreezeModeDesign, BreezeModeDirection)
}

// ValidateBreezeReferences enforces the Breeze worker's decoded-byte limits
// (breezeMaxReferenceBytes, breezeMaxTotalBytes) before anything is encoded or
// sent. Timbre's upload cap is 10 MB — wider than the worker's 4 MiB per clip —
// so an accepted upload can still be rejected here, with a specific reason,
// exactly as it is for Higgs today.
func ValidateBreezeReferences(refs []BreezeReference) error {
	var total int
	for i, ref := range refs {
		if len(ref.Audio) > breezeMaxReferenceBytes {
			return &BreezeValidationError{
				Reason: fmt.Sprintf("reference %d is %d bytes decoded (max %d)", i, len(ref.Audio), breezeMaxReferenceBytes),
			}
		}
		if len(ref.Audio) == 0 {
			return &BreezeValidationError{Reason: fmt.Sprintf("reference %d is empty", i)}
		}
		total += len(ref.Audio)
	}
	if total > breezeMaxTotalBytes {
		return &BreezeValidationError{
			Reason: fmt.Sprintf("total reference audio is %d bytes decoded (max %d)", total, breezeMaxTotalBytes),
		}
	}
	return nil
}

// ValidateBreezeInput enforces everything the worker's schema validator would
// reject, before a request is spent finding out: a non-empty script
// (missing_required_field), a known mode (invalid_mode), the per-mode field
// matrix (missing_required_field / forbidden_field_for_mode) and the reference
// byte limits (reference_audio_too_large / reference_audio_total_too_large).
func ValidateBreezeInput(in BreezeInput) error {
	if strings.TrimSpace(in.Text) == "" {
		return &BreezeValidationError{Reason: "breeze request has no text"}
	}

	switch in.Mode {
	case BreezeModeClone:
		if len(in.References) == 0 {
			return &BreezeValidationError{Reason: "breeze clone mode requires reference audio"}
		}
		if strings.TrimSpace(in.ReferenceText) == "" {
			return &BreezeValidationError{Reason: "breeze clone mode requires reference_text (the reference transcript)"}
		}
	case BreezeModeDirection:
		if len(in.References) == 0 {
			return &BreezeValidationError{Reason: "breeze direction mode requires reference audio"}
		}
		if strings.TrimSpace(in.ReferenceText) == "" {
			return &BreezeValidationError{Reason: "breeze direction mode requires reference_text (the reference transcript)"}
		}
		if strings.TrimSpace(in.Instruct) == "" {
			return &BreezeValidationError{Reason: "breeze direction mode requires instruct"}
		}
	case BreezeModeDesign:
		if len(in.References) > 0 {
			return &BreezeValidationError{Reason: "breeze design mode forbids reference audio (the worker rejects it as forbidden_field_for_mode)"}
		}
		if strings.TrimSpace(in.Instruct) == "" {
			return &BreezeValidationError{Reason: "breeze design mode requires instruct"}
		}
	default:
		return &BreezeValidationError{Reason: breezeUnknownModeReason(in.Mode)}
	}

	return ValidateBreezeReferences(in.References)
}


// AuKModel is the RunPod model identifier recorded in jobs.model.
const AuKModel = "tencent/AuK"

// AuK task names are the worker's closed schema_validator.py set.
const (
	AuKTaskAuto              = "auto"
	AuKTaskZeroShotTTS       = "zero_shot_tts"
	AuKTaskInstructTTS       = "instruct_tts"
	AuKTaskContentEdit       = "content_edit"
	AuKTaskAcousticEdit      = "acoustic_edit"
	AuKTaskParalinguisticEdit = "paralinguistic_edit"
	AuKTaskEnhancement       = "enhancement"
	AuKTaskSeparation        = "separation"

	AuKVariantFlash = "flash"
	AuKVariantBase  = "base"

	AuKDeliveryAuto   = "auto"
	AuKDeliveryS3     = "s3"
	AuKDeliveryBase64 = "base64"
)

// AuKMaxAudioBytes mirrors the worker's decoded per-clip limit.
const AuKMaxAudioBytes = 15 << 20

// AuKValidationError is a permanent caller-side payload failure.
type AuKValidationError struct {
	Reason string
}

func (e *AuKValidationError) Error() string { return "runpod: invalid AuK input: " + e.Reason }

// AuKInput is the worker's complete input object. Audio and PromptAudio are
// either base64/data URLs or HTTP(S) URLs; local files are encoded by the
// submission worker before this value reaches the client.
type AuKInput struct {
	Task             string
	Instruction      string
	Audio            string
	PromptAudio      string
	PromptText       string
	GenSeconds       float64
	ModelVariant     string
	NFE              int
	CfgScale         float64
	Seed             *int64
	ResponseDelivery string
}

func (in AuKInput) MarshalJSON() ([]byte, error) {
	if err := ValidateAuKInput(in); err != nil {
		return nil, err
	}
	payload := map[string]any{
		"task":              in.Task,
		"instruction":       in.Instruction,
		"model_variant":     in.ModelVariant,
		"nfe":               in.NFE,
		"cfg_scale":         in.CfgScale,
		"response_delivery": in.ResponseDelivery,
	}
	if in.Audio != "" {
		payload["audio"] = in.Audio
	}
	if in.PromptAudio != "" {
		payload["prompt_audio"] = in.PromptAudio
	}
	if in.PromptText != "" {
		payload["prompt_text"] = in.PromptText
	}
	if in.GenSeconds != 0 {
		payload["gen_seconds"] = in.GenSeconds
	}
	if in.Seed != nil {
		payload["seed"] = *in.Seed
	}
	return json.Marshal(payload)
}

// NormalizeAuKInput resolves the documented worker defaults so persisted jobs
// produce deterministic payloads even if endpoint defaults change later.
func NormalizeAuKInput(in AuKInput) AuKInput {
	if strings.TrimSpace(in.Task) == "" {
		in.Task = AuKTaskAuto
	}
	if strings.TrimSpace(in.ModelVariant) == "" {
		in.ModelVariant = AuKVariantFlash
	}
	if strings.TrimSpace(in.ResponseDelivery) == "" {
		in.ResponseDelivery = AuKDeliveryAuto
	}
	if in.ModelVariant == AuKVariantBase {
		if in.NFE == 0 {
			in.NFE = 32
		}
		if in.CfgScale == 0 {
			in.CfgScale = 2
		}
	} else {
		if in.NFE == 0 {
			in.NFE = 4
		}
		in.CfgScale = 0
	}
	return in
}

// ResolveAuKTask applies the worker's conservative auto-mode resolver.
func ResolveAuKTask(in AuKInput) (string, error) {
	task := strings.TrimSpace(in.Task)
	if task == "" || task == AuKTaskAuto {
		switch {
		case in.PromptAudio != "":
			return AuKTaskZeroShotTTS, nil
		case in.Audio != "":
			return "", &AuKValidationError{Reason: "auto mode cannot infer an edit task from source audio"}
		default:
			return AuKTaskInstructTTS, nil
		}
	}
	for _, allowed := range []string{
		AuKTaskZeroShotTTS, AuKTaskInstructTTS, AuKTaskContentEdit,
		AuKTaskAcousticEdit, AuKTaskParalinguisticEdit,
		AuKTaskEnhancement, AuKTaskSeparation,
	} {
		if task == allowed {
			return task, nil
		}
	}
	return "", &AuKValidationError{Reason: "unknown task " + strconv.Quote(task)}
}

func validateAuKAudio(value, field string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		return nil
	}
	encoded := value
	if strings.HasPrefix(encoded, "data:") {
		comma := strings.IndexByte(encoded, ',')
		if comma < 0 || !strings.Contains(encoded[:comma], ";base64") {
			return &AuKValidationError{Reason: field + " must be base64, a base64 data URL, or an HTTP(S) URL"}
		}
		encoded = encoded[comma+1:]
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 {
		return &AuKValidationError{Reason: field + " is not valid base64 or an HTTP(S) URL"}
	}
	if len(decoded) > AuKMaxAudioBytes {
		return &AuKValidationError{Reason: fmt.Sprintf("%s is %d bytes decoded (max %d)", field, len(decoded), AuKMaxAudioBytes)}
	}
	return nil
}

// ValidateAuKInput mirrors the worker's fail-fast task matrix and numeric
// bounds. The first failing rule wins.
func ValidateAuKInput(in AuKInput) error {
	in = NormalizeAuKInput(in)
	if strings.TrimSpace(in.Instruction) == "" {
		return &AuKValidationError{Reason: "instruction is required"}
	}
	if err := validateAuKAudio(in.Audio, "audio"); err != nil {
		return err
	}
	if err := validateAuKAudio(in.PromptAudio, "prompt_audio"); err != nil {
		return err
	}
	task, err := ResolveAuKTask(in)
	if err != nil {
		return err
	}
	switch task {
	case AuKTaskZeroShotTTS:
		if in.PromptAudio == "" {
			return &AuKValidationError{Reason: "zero_shot_tts requires prompt_audio"}
		}
		if in.Audio != "" {
			return &AuKValidationError{Reason: "zero_shot_tts forbids audio"}
		}
		// The worker leaves gen_seconds at nil when it is not given and
		// reference/prompt audio is present, which it documents as
		// "intentionally preserves the source duration" — for zero-shot
		// that source is the voice reference clip, not the target phrase,
		// so an unhinted request has been observed making the model speak
		// the reference clip's own content instead. Confirmed against the
		// official Tencent-Hunyuan/AuK cookbook, which always pairs a
		// zero-shot --instruction with an explicit --gen_seconds.
		if in.GenSeconds == 0 {
			return &AuKValidationError{Reason: "zero_shot_tts requires gen_seconds to hint the target duration"}
		}
	case AuKTaskInstructTTS:
		if in.Audio != "" || in.PromptAudio != "" {
			return &AuKValidationError{Reason: "instruct_tts forbids audio and prompt_audio"}
		}
		// instruct_tts has no audio at all, so the worker falls back to a
		// flat 7-second default when gen_seconds is unset — decoupled from
		// the actual target text length. The cookbook always pairs this
		// task's --instruction with an explicit --gen_seconds too.
		if in.GenSeconds == 0 {
			return &AuKValidationError{Reason: "instruct_tts requires gen_seconds to hint the target duration"}
		}
	case AuKTaskContentEdit, AuKTaskAcousticEdit, AuKTaskParalinguisticEdit, AuKTaskEnhancement, AuKTaskSeparation:
		if in.Audio == "" {
			return &AuKValidationError{Reason: task + " requires audio"}
		}
		if in.PromptAudio != "" {
			return &AuKValidationError{Reason: task + " forbids prompt_audio"}
		}
	}
	if in.PromptText != "" && in.PromptAudio == "" {
		return &AuKValidationError{Reason: "prompt_text is valid only with prompt_audio"}
	}
	if in.GenSeconds != 0 && (in.GenSeconds < 0.5 || in.GenSeconds > 300) {
		return &AuKValidationError{Reason: "gen_seconds must be between 0.5 and 300"}
	}
	switch in.ModelVariant {
	case AuKVariantFlash:
		if in.NFE < 1 || in.NFE > 8 {
			return &AuKValidationError{Reason: "flash nfe must be between 1 and 8"}
		}
		if in.CfgScale != 0 {
			return &AuKValidationError{Reason: "flash cfg_scale must be 0"}
		}
	case AuKVariantBase:
		if in.NFE < 16 || in.NFE > 64 {
			return &AuKValidationError{Reason: "base nfe must be between 16 and 64"}
		}
		if in.CfgScale < 1 || in.CfgScale > 5 {
			return &AuKValidationError{Reason: "base cfg_scale must be between 1 and 5"}
		}
	default:
		return &AuKValidationError{Reason: "model_variant must be flash or base"}
	}
	if in.Seed != nil && *in.Seed < 0 {
		return &AuKValidationError{Reason: "seed must be non-negative"}
	}
	switch in.ResponseDelivery {
	case AuKDeliveryAuto, AuKDeliveryS3, AuKDeliveryBase64:
	default:
		return &AuKValidationError{Reason: "response_delivery must be auto, s3 or base64"}
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
	// Each engine is a separately deployed RunPod Serverless endpoint. The
	// method called selects the endpoint; all engines share this client's HTTP
	// transport and bearer token.
	mossEndpoint   string
	higgsEndpoint  string
	breezeEndpoint string
	aukEndpoint    string
	apiKey         string
	http           *http.Client
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

// WithBreezeEndpoint sets the third endpoint SubmitBreeze and StatusBreeze
// route to (BREEZE_RUNPOD_ENDPOINT). Left unset, those calls fail with
// ErrNoBreezeEndpoint rather than falling back to the MOSS or Higgs endpoint.
func WithBreezeEndpoint(endpoint string) Option {
	return func(c *Client) { c.breezeEndpoint = strings.TrimRight(endpoint, "/") }
}


// WithAuKEndpoint sets the dedicated Tencent AuK endpoint used by
// SubmitAuK, StatusAuK and HealthAuK.
func WithAuKEndpoint(endpoint string) Option {
	return func(c *Client) { c.aukEndpoint = strings.TrimRight(endpoint, "/") }
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


// AuKConfigured reports whether the dedicated AuK endpoint and shared key are present.
func (c *Client) AuKConfigured() bool {
	return c.aukEndpoint != "" && c.apiKey != ""
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

// SubmitBreeze posts a Breeze TTS 2 job to /run on the Breeze endpoint and
// returns the async id RunPod assigns. The input is validated against the
// worker's own schema rules (ValidateBreezeInput) before anything is encoded or
// sent, so a payload the worker would reject never costs a request.
func (c *Client) SubmitBreeze(ctx context.Context, in BreezeInput) (Submission, error) {
	if c.breezeEndpoint == "" {
		return Submission{}, ErrNoBreezeEndpoint
	}
	if err := ValidateBreezeInput(in); err != nil {
		return Submission{}, err
	}

	body, err := json.Marshal(map[string]any{"input": in})
	if err != nil {
		return Submission{}, fmt.Errorf("runpod: encode breeze submission: %w", err)
	}

	var out Submission
	if err := c.do(ctx, http.MethodPost, c.breezeEndpoint, "/run", body, &out); err != nil {
		return Submission{}, err
	}
	if out.ID == "" {
		return Submission{}, errors.New("runpod: /run returned no job id")
	}
	return out, nil
}


// SubmitAuK queues one Tencent AuK generation/editing job.
func (c *Client) SubmitAuK(ctx context.Context, in AuKInput) (Submission, error) {
	if c.aukEndpoint == "" {
		return Submission{}, ErrNoAuKEndpoint
	}
	in = NormalizeAuKInput(in)
	if err := ValidateAuKInput(in); err != nil {
		return Submission{}, err
	}
	body, err := json.Marshal(map[string]any{"input": in})
	if err != nil {
		return Submission{}, fmt.Errorf("runpod: encode AuK submission: %w", err)
	}
	var out Submission
	if err := c.do(ctx, http.MethodPost, c.aukEndpoint, "/run", body, &out); err != nil {
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


// HealthAuK queries the dedicated Tencent AuK endpoint health snapshot.
func (c *Client) HealthAuK(ctx context.Context) (Health, error) {
	if c.aukEndpoint == "" {
		return Health{}, ErrNoAuKEndpoint
	}
	var out Health
	if err := c.do(ctx, http.MethodGet, c.aukEndpoint, "/health", nil, &out); err != nil {
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

	// The remaining fields are the Breeze worker's completion metadata
	// (sruckh/breezetts-runpod). They are additive and omitempty: MOSS and
	// Higgs never send them, so they stay at their zero value on those engines
	// and no existing parsing changes. Breeze's audio and its 24000 Hz rate
	// need no new field — AudioBase64 and SampleRate above already carry them,
	// and Breeze sends no `format` (the poller's "wav" default applies).
	//
	// Delivery must read BreezeDeliveryBase64 on every completion; anything
	// else means the worker ignored the pinned response_delivery and the audio
	// is not inline.
	Delivery        string  `json:"delivery,omitempty"`
	AudioURL        string  `json:"audio_url,omitempty"`
	Bucket          string  `json:"bucket,omitempty"`
	Key             string  `json:"key,omitempty"`
	SizeBytes       int64   `json:"size_bytes,omitempty"`
	Mode            string  `json:"mode,omitempty"`
	CfgScale        float64 `json:"cfg_scale,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	ModelVariant    string  `json:"model_variant,omitempty"`
	NFE             int     `json:"nfe,omitempty"`
	TaskExecuted    string  `json:"task_executed,omitempty"`
	URLExpiresIn    int64   `json:"url_expires_in,omitempty"`
	URLExpiresAt    string  `json:"url_expires_at,omitempty"`

	// Error carries a worker failure envelope that arrived nested inside
	// `output` rather than at the top level of the status response. RunPod's
	// own runtime lifts a handler-returned "error" key to StatusResult.Error,
	// so this is the defensive half of BreezeError. json.RawMessage accepts any
	// shape, so it can never turn into a permanent DecodeError.
	Error json.RawMessage `json:"error,omitempty"`
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

// BreezeWorkerError is the Breeze worker's structured failure envelope,
// {"error":{"code","message","field"}} (schema_validator.py:27, :33). Code is
// one of the worker's nine documented codes — invalid_payload,
// missing_required_field, invalid_mode, forbidden_field_for_mode,
// invalid_base64, reference_audio_too_large, reference_audio_total_too_large,
// invalid_cfg_scale, invalid_response_delivery — plus whatever synthesis and
// delivery failures report through the same envelope. Field is omitted when the
// failure is not field-scoped. The envelope never carries a stack trace, a
// credential or raw reference audio, so it is safe to persist verbatim as a
// job's failure reason.
type BreezeWorkerError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func (e *BreezeWorkerError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "the Breeze worker rejected the request"
	}
	switch {
	case e.Code == "":
		return "runpod: breeze: " + msg
	case e.Field == "":
		return fmt.Sprintf("runpod: breeze %s: %s", e.Code, msg)
	default:
		return fmt.Sprintf("runpod: breeze %s (%s): %s", e.Code, e.Field, msg)
	}
}

// BreezeError extracts the Breeze worker's failure envelope from a status
// response, or nil when the response carries none. It reads the top level
// first — RunPod's runtime lifts a handler-returned "error" key out of the
// output object — and falls back to Output.Error for the nested form.
//
// Whether a given failure is worth retrying is the caller's decision, not this
// package's: a status response that reached Timbre at all is a completed
// round-trip, and the poller already fails a FAILED job outright.
func (sr StatusResult) BreezeError() *BreezeWorkerError {
	if sr.Error != nil {
		raw, err := json.Marshal(sr.Error)
		if err == nil {
			if env := decodeBreezeError(raw); env != nil {
				return env
			}
		}
	}
	return decodeBreezeError(sr.Output.Error)
}

// decodeBreezeError accepts the envelope bare ({"code":...}) or still wrapped
// ({"error":{"code":...}}), and returns nil for any other shape — a plain
// string error from RunPod itself is not a Breeze envelope.
func decodeBreezeError(raw json.RawMessage) *BreezeWorkerError {
	if len(raw) == 0 {
		return nil
	}
	var wrapped struct {
		Error *BreezeWorkerError `json:"error"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Error != nil {
		if wrapped.Error.Code != "" || wrapped.Error.Message != "" {
			return wrapped.Error
		}
	}
	var env BreezeWorkerError
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	if env.Code == "" && env.Message == "" {
		return nil
	}
	return &env
}


// AuKWorkerError is the structured envelope serialized into RunPod's string
// error field by sruckh/tencent-auk.
type AuKWorkerError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func (e *AuKWorkerError) Error() string {
	if e == nil {
		return ""
	}
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = "AuK execution failed"
	}
	if e.Field != "" {
		message += " (" + e.Field + ")"
	}
	if e.Code != "" {
		return e.Code + ": " + message
	}
	return message
}

// AuKError decodes both RunPod's required JSON string and a defensive nested
// output.error envelope.
func (sr StatusResult) AuKError() *AuKWorkerError {
	if sr.Error != nil {
		if env := decodeAuKErrorValue(sr.Error); env != nil {
			return env
		}
	}
	if len(sr.Output.Error) > 0 {
		var value any
		if err := json.Unmarshal(sr.Output.Error, &value); err == nil {
			return decodeAuKErrorValue(value)
		}
	}
	return nil
}

func decodeAuKErrorValue(value any) *AuKWorkerError {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil {
		raw = []byte(encoded)
	}
	var wrapped struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Error) > 0 {
		raw = wrapped.Error
		if err := json.Unmarshal(raw, &encoded); err == nil {
			raw = []byte(encoded)
		}
	}
	var env AuKWorkerError
	if err := json.Unmarshal(raw, &env); err != nil || (env.Code == "" && env.Message == "") {
		return nil
	}
	return &env
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

// StatusBreeze queries GET /status/{id} on the Breeze endpoint. Mirrors Status
// and StatusHiggs, each of which stays pinned to its own endpoint — a job's
// engine determines which method the caller uses to poll it.
func (c *Client) StatusBreeze(ctx context.Context, id string) (StatusResult, error) {
	if id == "" {
		return StatusResult{}, errors.New("runpod: empty job id")
	}
	if c.breezeEndpoint == "" {
		return StatusResult{}, ErrNoBreezeEndpoint
	}
	var out StatusResult
	if err := c.do(ctx, http.MethodGet, c.breezeEndpoint, "/status/"+id, nil, &out); err != nil {
		return StatusResult{}, err
	}
	return out, nil
}


// StatusAuK reads one job from the dedicated Tencent AuK endpoint.
func (c *Client) StatusAuK(ctx context.Context, id string) (StatusResult, error) {
	if id == "" {
		return StatusResult{}, errors.New("runpod: empty job id")
	}
	if c.aukEndpoint == "" {
		return StatusResult{}, ErrNoAuKEndpoint
	}
	var out StatusResult
	if err := c.do(ctx, http.MethodGet, c.aukEndpoint, "/status/"+id, nil, &out); err != nil {
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
