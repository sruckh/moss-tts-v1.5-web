package web

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/voices"
)

// render is the shared harness: a component in, its HTML out.
func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var sb strings.Builder
	if err := c.Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// rowHTML returns the markup of the <tr> carrying the given id, so a test can
// assert about one row rather than the whole table.
func rowHTML(t *testing.T, html, id string) string {
	t.Helper()
	at := strings.Index(html, `id="`+id+`"`)
	if at < 0 {
		t.Fatalf("row %s not found", id)
	}
	start := strings.LastIndex(html[:at], "<tr")
	end := strings.Index(html[at:], "</tr>")
	if start < 0 || end < 0 {
		t.Fatalf("row %s is not inside a <tr>", id)
	}
	return html[start : at+end]
}

// sampleJobs is a queue with one of each state, newest first — the shape the
// handlers pass in.
func sampleJobs() []jobs.Job {
	return []jobs.Job{
		{ID: 9, UserID: 1, Status: jobs.StatusReady, VoiceID: 1, VoiceName: "Moss", VoiceKind: voices.KindStock,
			Text: "Welcome back. Your last render finished.", AudioPath: "/x.wav", Format: "wav",
			SampleRate: 24000, ExecMS: 4100, Model: jobs.DefaultModel},
		{ID: 8, UserID: 1, Status: jobs.StatusInProgress, VoiceID: 2, VoiceName: "Legacy", VoiceKind: voices.KindStock,
			Text: "Chapter two, full read.", Model: jobs.DefaultModel},
		{ID: 7, UserID: 1, Status: jobs.StatusFailed, VoiceID: 1, VoiceName: "Moss", VoiceKind: voices.KindStock,
			Text: "Outro.", Error: "endpoint rejected the submission", Model: jobs.DefaultModel},
	}
}

func sampleVoices() []voices.Voice {
	return []voices.Voice{
		{ID: 1, Kind: voices.KindStock, Name: "Moss", Model: "MOSS-TTS v1.5", LicenseLabel: "OpenMOSS Community"},
		{ID: 4, Kind: voices.KindCloned, Name: "Marrow", Model: "Cloned", LicenseLabel: "Cloned voice", CanDelete: true},
	}
}

// The queue must fit the viewport. A fixed-layout, full-width table with
// wrapping cells is what buys that; a horizontal scroller is the thing being
// removed, and it is also what the two-second poll kept resetting to the left.
func TestQueueTableHasNoHorizontalScroller(t *testing.T) {
	html := render(t, Queue(sampleJobs(), 0, map[int64]string{9: "0:06.02"}, 0))

	for _, banned := range []string{"overflow-x", "min-w-"} {
		if strings.Contains(html, banned) {
			t.Errorf("queue fragment still contains %q — the table must fit, not scroll", banned)
		}
	}
	for _, want := range []string{`class="queue-table w-full table-fixed"`, `role="grid"`} {
		if !strings.Contains(html, want) {
			t.Errorf("queue table missing %q", want)
		}
	}
}

// The download control is a glyph with an accessible name, not a phrase: the
// words are what pushed the table past the viewport.
func TestQueueDownloadControlIsIconOnly(t *testing.T) {
	html := render(t, Queue(sampleJobs(), 0, map[int64]string{9: "0:06.02"}, 0))

	if strings.Contains(html, "Download WAV") {
		t.Error("queue row still carries the text 'Download WAV'")
	}
	if !strings.Contains(html, `aria-label="Download take 9 as WAV"`) {
		t.Error("download control has no accessible name")
	}
	if !strings.Contains(html, "<svg") {
		t.Error("download control renders no icon")
	}
	if !strings.Contains(html, `aria-label="Delete take 9"`) {
		t.Error("delete control has no accessible name")
	}
}

// Selection is server-rendered so it survives the swap: exactly the selected
// row carries it.
func TestQueueMarksSelectedRow(t *testing.T) {
	html := render(t, Queue(sampleJobs(), 0, nil, 8))

	if strings.Count(html, `aria-selected="true"`) != 1 {
		t.Errorf("want exactly one selected row, got %d", strings.Count(html, `aria-selected="true"`))
	}
	row := rowHTML(t, html, "job-8")
	if !strings.Contains(row, `aria-selected="true"`) || !strings.Contains(row, "is-selected") {
		t.Errorf("row job-8 is not marked selected: %s", row)
	}
	// The chip ships on every row and is revealed by the class, so the word and
	// the wash can never disagree.
	if !strings.Contains(row, "In the player") || !strings.Contains(row, "queue-mark") {
		t.Errorf("row job-8 carries no selection marker: %s", row)
	}
	other := rowHTML(t, html, "job-9")
	if strings.Contains(other, `aria-selected="true"`) || strings.Contains(other, "is-selected") {
		t.Errorf("row job-9 should not be selected: %s", other)
	}
	if !strings.Contains(html, `hx-get="/jobs/8/player"`) {
		t.Error("queue row does not load its take into the player")
	}
}

func TestQueueRowsPublishReadySourceSelection(t *testing.T) {
	html := render(t, Queue(sampleJobs(), 0, nil, 9))
	ready := rowHTML(t, html, "job-9")
	if !strings.Contains(ready, `data-source-ready="true"`) {
		t.Errorf("ready row does not publish source readiness: %s", ready)
	}
	working := rowHTML(t, html, "job-8")
	if !strings.Contains(working, `data-source-ready="false"`) {
		t.Errorf("working row incorrectly advertises ready audio: %s", working)
	}
	page := render(t, Studio(sampleJobs(), sampleVoices(), nil, 9))
	if !strings.Contains(page, `timbre-source-selected`) {
		t.Error("studio helper does not publish selected queue rows to the compose form")
	}
}

// The poll asks for the selected take by URL, so every swap re-renders the
// highlight and the fragment that arrives asks for it again — the selection
// sustains itself across a table that is replaced every two seconds.
func TestQueuePollCarriesSelectedTake(t *testing.T) {
	selected := render(t, Queue(sampleJobs(), 0, nil, 9))
	if !strings.Contains(selected, `hx-get="/jobs/queue?take=9"`) {
		t.Error("the queue poll does not ask for the selected take")
	}

	none := render(t, Queue(sampleJobs(), 0, nil, 0))
	if !strings.Contains(none, `hx-get="/jobs/queue"`) {
		t.Error("the unselected queue should poll the plain URL")
	}
	if strings.Contains(none, "is-selected") {
		t.Error("a queue with no selection marked a row selected")
	}
}

// The player lives outside the polled fragment. If an <audio> element ever
// appeared in the queue, the two-second swap would restart playback.
func TestQueueFragmentCarriesNoPlayer(t *testing.T) {
	html := render(t, Queue(sampleJobs(), 0, map[int64]string{9: "0:06.02"}, 9))

	if strings.Contains(html, "<audio") {
		t.Error("queue fragment contains an <audio> element; the 2s poll would restart playback")
	}
}

// The player names the model that rendered the take, read from the row.
func TestPlayerShowsStoredModelBadge(t *testing.T) {
	ready := sampleJobs()[0]
	ready.Model = "MOSS-TTS v9.9"
	html := render(t, PlayerBody(ready, "0:06.02"))

	if !strings.Contains(html, "badge--info") {
		t.Error("player has no informational badge for the model")
	}
	if !strings.Contains(html, "MOSS-TTS v9.9") {
		t.Errorf("player does not name the stored model: %s", html)
	}
}

// A take that is not ready says what it is doing instead of showing transport
// controls that cannot work.
func TestPlayerBodyStates(t *testing.T) {
	items := sampleJobs()

	if html := render(t, PlayerBody(items[1], "")); strings.Contains(html, "<audio") {
		t.Error("a rendering take must not get an audio element")
	}
	if html := render(t, PlayerBody(items[2], "")); !strings.Contains(html, "endpoint rejected the submission") {
		t.Error("a failed take must show its recorded reason")
	}
	if html := render(t, PlayerBody(jobs.Job{}, "")); !strings.Contains(html, "Nothing to play yet") {
		t.Error("the zero take must render the empty state")
	}
}

// Clearing the script is one control, not a select-all.
func TestComposeHasClearScriptControl(t *testing.T) {
	html := render(t, Compose(sampleVoices(), 1, jobs.Job{}))

	if !strings.Contains(html, "Clear script") {
		t.Error("compose card has no clear-script control")
	}
	if !strings.Contains(html, `x-ref="scriptBox"`) {
		t.Error("the script textarea is not addressable by the clear control")
	}
	if !strings.Contains(html, `type="button"`) {
		t.Error("the clear control must not submit the form")
	}
}

func TestComposeEngineSelector(t *testing.T) {
	html := render(t, Compose(sampleVoices(), 1, jobs.Job{}))

	for _, want := range []string{
		`name="model"`,
		`aria-label="Speech engine"`,
		`focus:ring-2`,
		`value="MOSS-TTS v1.5" selected`,
		`value="bosonai/higgs-tts-3-4b"`,
		`value="BreezeBlue/Breeze-TTS-2"`,
		`value="tencent/AuK"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("engine selector missing %q", want)
		}
	}
}

func TestQueueAnnouncesUpdatesAndShowsModel(t *testing.T) {
	items := sampleJobs()
	items[0].Model = jobs.HiggsModel
	html := render(t, Queue(items, 0, nil, 0))

	for _, want := range []string{
		`hx-get="/jobs/queue"`,
		`aria-live="polite"`,
		`badge--info`,
		jobs.HiggsModel,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("queue missing %q", want)
		}
	}
	if strings.Contains(html, "<audio") {
		t.Error("announced queue fragment contains player audio")
	}
}

// A cloned voice card can be renamed and previewed; a stock card offers
// neither, because Rename refuses stock voices and they have no reference.
func TestVoiceCardControls(t *testing.T) {
	html := render(t, VoiceGrid(sampleVoices(), 4))

	for _, want := range []string{
		`hx-post="/voices/4/name"`,
		`src="/voices/4/reference"`,
		`aria-label="Rename Marrow"`,
		`hx-delete="/voices/4"`,
		`aria-label="Delete Marrow"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("cloned card missing %q", want)
		}
	}
	if strings.Contains(html, `hx-post="/voices/1/name"`) {
		t.Error("stock card offers a rename it cannot perform")
	}
	if strings.Contains(html, `src="/voices/1/reference"`) {
		t.Error("stock card offers a reference preview it does not have")
	}
	if strings.Contains(html, `hx-delete="/voices/1"`) {
		t.Error("stock card offers deletion")
	}
	withoutAuthority := []voices.Voice{{ID: 5, Kind: voices.KindCloned, Name: "Shared", Model: "Cloned"}}
	if got := render(t, VoiceGrid(withoutAuthority, 5)); strings.Contains(got, `hx-delete="/voices/5"`) {
		t.Error("assigned non-owner card offers deletion")
	}
	// A card holds buttons now, so it may not be one itself.
	if strings.Contains(html, `<button type="button" aria-pressed=`) {
		t.Error("the voice card is still a button and now nests buttons")
	}
}

func TestVoiceGridSwapReconcilesDeletedSelection(t *testing.T) {
	html := render(t, Studio(sampleJobs(), sampleVoices(), nil, 0))
	for _, want := range []string{
		"function timbrePaintVoice()",
		"cards.find(function (card) { return card.dataset.voiceId === vid.value; })",
		"timbrePaintVoice();",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("studio selection reconciliation missing %q", want)
		}
	}
}

func TestAdminVoiceDeleteControlIsClonedOnly(t *testing.T) {
	html := render(t, AdminPanel(AdminData{Voices: []AdminVoice{
		{ID: 1, Kind: voices.KindStock, Name: "Moss"},
		{ID: 2, Kind: voices.KindCloned, Name: "Disposable"},
	}}))
	if !strings.Contains(html, `hx-delete="/admin/voices/2"`) {
		t.Error("admin clone row is missing delete control")
	}
	if strings.Contains(html, `hx-delete="/admin/voices/1"`) {
		t.Error("admin stock row offers deletion")
	}
}

func TestVoiceCardTranscriptionReadiness(t *testing.T) {
	items := []voices.Voice{
		{ID: 1, Kind: voices.KindStock, Name: "Moss", Model: jobs.DefaultModel, LicenseLabel: "OpenMOSS Community"},
		{ID: 2, Kind: voices.KindCloned, Name: "Missing", ReferenceTranscript: sql.Null[string]{}},
		{ID: 3, Kind: voices.KindCloned, Name: "Blank", ReferenceTranscript: sql.Null[string]{V: "  ", Valid: true}},
		{ID: 4, Kind: voices.KindCloned, Name: "Ready", ReferenceTranscript: sql.Null[string]{V: "Reference words.", Valid: true}},
	}
	html := render(t, VoiceGrid(items, 2))

	if got := strings.Count(html, "Transcribing..."); got != 2 {
		t.Errorf("Transcribing badge count = %d, want 2", got)
	}
	if got := strings.Count(html, ">Ready</span>"); got != 1 {
		t.Errorf("Ready transcription badge count = %d, want 1", got)
	}
	for _, want := range []string{`hx-get="/voices/grid"`, `hx-trigger="every 3s"`, `hx-swap="outerHTML show:none"`} {
		if !strings.Contains(html, want) {
			t.Errorf("pending grid missing %q", want)
		}
	}

	readyHTML := render(t, VoiceGrid([]voices.Voice{items[0], items[3]}, 4))
	if strings.Contains(readyHTML, `hx-get="/voices/grid"`) {
		t.Error("fully ready grid keeps polling")
	}
	if !strings.Contains(html, "focus:ring-2") {
		t.Error("voice cards lost their keyboard focus ring")
	}
}

// When a take carries forced alignment, the spoken line renders one timed span
// per word with data-t0/data-t1 (so the playhead tracks real speech from the
// model-normalized words). Without alignment it emits no timing attributes and
// the client falls back to proportional interpolation across the script's words.
func TestPlayerWordTimings(t *testing.T) {
	ready := sampleJobs()[0] // a ready take

	// With alignment: timed spans, rendered from the alignment's own words.
	ready.AlignmentJSON = `{"frame_rate":50,"source":"mms_fa_forced_alignment","words":[{"w":"Welcome","start":0.00,"end":0.30},{"w":"back.","start":0.32,"end":0.55}]}`
	html := render(t, PlayerBody(ready, "0:06.02"))
	if !strings.Contains(html, `data-t0=`) || !strings.Contains(html, `data-t1=`) {
		t.Errorf("aligned take did not emit data-t0/data-t1 word spans:\n%s", html)
	}
	if !strings.Contains(html, ">Welcome<") || !strings.Contains(html, ">back.<") {
		t.Errorf("aligned take did not render the alignment's own words:\n%s", html)
	}

	// Without alignment: no timing attributes — pure interpolation fallback.
	ready.AlignmentJSON = ""
	html = render(t, PlayerBody(ready, "0:06.02"))
	if strings.Contains(html, `data-t0=`) {
		t.Errorf("unaligned take emitted data-t0; it must fall back to interpolation:\n%s", html)
	}
	if !strings.Contains(html, ">Welcome<") {
		t.Errorf("unaligned take did not render the script's words:\n%s", html)
	}
}

// The queue's scroll container must carry overflow-anchor:none. The 2s poll
// replaces every row in one outerHTML swap, and Chromium's scroll anchoring
// recomputes against the fresh nodes — snapping any non-top scroll position
// straight to the bottom (reproduced in Chromium/Brave: 500px -> max on the
// first tick; with the property set the offset holds indefinitely).
func TestScrollableListDisablesScrollAnchoring(t *testing.T) {
	data, err := os.ReadFile("app.css")
	if err != nil {
		t.Fatalf("read app.css: %v (did the Tailwind build step run?)", err)
	}
	css := string(data)
	i := strings.Index(css, ".scrollable-list {")
	if i < 0 {
		i = strings.Index(css, ".scrollable-list{")
	}
	if i < 0 {
		t.Fatal("app.css has no .scrollable-list rule")
	}
	rule := css[i : i+strings.Index(css[i:], "}")+1]
	if !strings.Contains(rule, "overflow-anchor") {
		t.Errorf(".scrollable-list rule is missing overflow-anchor:none: %s", rule)
	}
}

// The queue's vertical scroll container must live OUTSIDE the swapped #queue
// fragment: the 2s poll replaces #queue with hx-swap="outerHTML", so any
// element inside it that holds a scroll offset is recreated on every tick and
// the scroll snaps back to the top. The fragment therefore carries no
// scrollable-list of its own; the pages embedding Queue provide a static
// wrapper that HTMX never touches.
func TestQueueScrollContainerIsOutsideSwappedFragment(t *testing.T) {
	fragment := render(t, Queue(sampleJobs(), 0, nil, 0))

	for _, banned := range []string{"scrollable-list", "max-h-[500px]", "hx-on"} {
		if strings.Contains(fragment, banned) {
			t.Errorf("queue fragment contains %q — scroll/JS state inside the swapped fragment is reset by every poll", banned)
		}
	}
	if !strings.Contains(fragment, `hx-swap="outerHTML show:none"`) {
		t.Error("queue fragment missing 'show:none' swap modifier — without it every 2s poll scrolls #queue into view, dragging the page scrollbar to the bottom")
	}
	if !strings.Contains(fragment, `hx-swap="innerHTML show:none"`) {
		t.Error("queue row's player load missing 'show:none' — selecting a take must not scroll the page to the player")
	}
	if got := strings.Count(fragment, `hx-swap="outerHTML show:none"`); got < 2 {
		t.Errorf("expected the poll AND every row's delete button to swap with show:none, found %d outerHTML show:none swaps", got)
	}

	for name, page := range map[string]templ.Component{
		"Studio":    Studio(sampleJobs(), sampleVoices(), nil, 0),
		"QueuePage": QueuePage(sampleJobs(), sampleVoices(), nil, 0),
	} {
		html := render(t, page)
		wrap := strings.Index(html, "scrollable-list max-h-[500px]")
		queue := strings.Index(html, `id="queue"`)
		if wrap < 0 {
			t.Errorf("%s is missing the static 'scrollable-list max-h-[500px]' queue wrapper", name)
			continue
		}
		if queue < 0 || wrap > queue {
			t.Errorf("%s must open the scroll wrapper before #queue (it has to be an ancestor HTMX never swaps)", name)
		}
	}
}

// The VoiceGrid container must be scrollable with max-height corresponding to 10 items.
func TestVoiceGridContainerIsScrollableWithMaxHeight(t *testing.T) {
	html := render(t, VoiceGrid(sampleVoices(), 1))

	if !strings.Contains(html, "scrollable-list") {
		t.Error("voice grid container missing class 'scrollable-list'")
	}
	if !strings.Contains(html, "max-h-[670px]") {
		t.Error("voice grid container missing max height constraint 'max-h-[670px]'")
	}
}

func TestComposeAuKExposesCompleteTaskContract(t *testing.T) {
	html := render(t, Compose(sampleVoices(), 1, sampleJobs()[0]))
	for _, want := range []string{
		`enctype="multipart/form-data"`, `value="tencent/AuK"`,
		`name="instruction"`, `name="task"`, `value="auto"`,
		`value="zero_shot_tts"`, `value="instruct_tts"`,
		`value="content_edit"`, `value="acoustic_edit"`,
		`value="paralinguistic_edit"`, `value="enhancement"`, `value="separation"`,
		`name="audio_source"`, `value="upload"`, `value="render"`,
		`name="audio_file"`, `name="source_job_id"`, `name="prompt_text"`,
		`selected cloned voice card`, `Use selected render`, `Take 9`,
		`name="gen_seconds"`, `id="auk_text_hint"`, `name="model_variant"`,
		`value="flash"`, `value="base"`, `name="nfe"`, `name="cfg_scale"`,
		`name="seed"`, `name="response_delivery"`, `value="s3"`, `value="base64"`,
		`decoded limit 15 MB`, `Run AuK task`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("AuK compose contract missing %q", want)
		}
	}
	for _, forbidden := range []string{`name="audio"`, `name="prompt_audio"`, `name="prompt_audio_file"`, `name="gen_text"`} {
		if strings.Contains(html, forbidden) {
			t.Errorf("AuK compose contract still exposes %q", forbidden)
		}
	}
}

func TestComposeExposesAuKPromptAssistant(t *testing.T) {
	html := render(t, Compose(sampleVoices(), 1, jobs.Job{}))
	for _, want := range []string{
		`x-data="aukAssistant()"`,
		`Ask the AI prompt assistant`,
		`Ask assistant`,
		`Clear conversation`,
		`Insert into instruction`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("AuK assistant panel missing %q", want)
		}
	}
	// The parent form must listen for the assistant's apply event and route
	// it into both the instruction field and the Mode selector — a reply
	// whose instruction says "using the reference voice provided" is useless
	// if the task stays on whatever the form defaulted to (see regression:
	// an inserted zero-shot instruction under instruct_tts sends AuK no
	// reference audio at all, since instruct_tts forbids it outright).
	if !strings.Contains(html, `x-on:timbre-assistant-apply.window="aukInstruction = $event.detail.instruction; aukInstructionTouched = true; if ($event.detail.task) { aukTask = $event.detail.task }; if ($event.detail.text) { aukTextHint = $event.detail.text; genSecondsTouched = false }"`) {
		t.Error("compose form does not wire the assistant's apply event into aukInstruction, aukTask and aukTextHint")
	}
	if !strings.Contains(html, `@click="apply(m.instruction, m.task)"`) {
		t.Error("insert-into-instruction button does not forward the parsed task alongside the instruction")
	}

	// The fetch target, the aukAssistant() factory and its task parser all
	// live in the shared studioHelpers script, rendered once per page
	// alongside Compose rather than inside it — same split as
	// timbre-source-selected.
	page := render(t, Studio(sampleJobs(), sampleVoices(), nil, 9))
	for _, want := range []string{
		`window.aukAssistant`, `/jobs/auk-assistant`, `timbre-assistant-apply`,
		`parseTask:`, `zero_shot_tts`, `instruct_tts`, `content_edit`,
		`acoustic_edit`, `paralinguistic_edit`, `separation`, `enhancement`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("studioHelpers script missing %q", want)
		}
	}

	// Regression history, each confirmed against a real reply that broke
	// the prior approach:
	//   1. A naive [^"]* character class truncates on the first quote,
	//      escaped or not, inside the instruction's own quoted phrase
	//      (say: "...").
	//   2. Bounding the capture by "the next known field, or a closing
	//      fence, or end of text" over-captures into free-form follow-up
	//      prose whenever a reply breaks format and omits those later
	//      fields (e.g. a mixed "here's the block, but I still need X"
	//      reply) — that garbled, self-contradictory text is what actually
	//      got submitted to AuK and is the likely reason it echoed the
	//      reference clip instead of the target phrase.
	// The fix must bound the value to its own single line — the fixed
	// format never wraps INSTRUCTION across lines — and must strip
	// delimiters only from the exact first/last character, never by
	// hunting for a "matching" quote inside the value.
	if strings.Contains(page, `/INSTRUCTION:\s*"([^"]*)"/`) {
		t.Error("parseInstruction regressed to the naive pattern that truncates on any quote")
	}
	if strings.Contains(page, `(?:\\.|[^"\\])*`) {
		t.Error("parseInstruction regressed to character-class quote-matching, which cannot survive an unescaped inner quote")
	}
	if strings.Contains(page, `(?:TASK|REQUIRED AUDIO INPUT|NOTES)\s*:|`+"`"+"`"+"`"+`|\$\)/`) {
		t.Error("parseInstruction regressed to bounding by a later field/fence/end-of-text, which over-captures trailing prose when a reply omits them")
	}
	if !strings.Contains(page, `/INSTRUCTION:\s*(.+)/`) {
		t.Error("parseInstruction does not bound the INSTRUCTION value to its own single line")
	}
	if !strings.Contains(page, `pairs = [['"', '"'], ["'", "'"], ['\u201c', '\u201d'], ['\u2018', '\u2019']]`) {
		t.Error("parseInstruction does not strip a known delimiter pair from exactly the first/last character")
	}
	if !strings.Contains(page, `.replace(/\\(["\\nrt])/g`) {
		t.Error("parseInstruction does not unescape backslash sequences a properly-escaped reply may still carry")
	}
}

func TestComposeDisablesEngineSpecificFields(t *testing.T) {
	html := render(t, Compose(sampleVoices(), 1, jobs.Job{}))
	for _, want := range []string{
		`x-bind:disabled="!isAuK || !aukNeedsSource || audioSource !== 'upload'"`,
		`x-bind:disabled="!isAuK || !aukNeedsSource || audioSource !== 'render' || !sourceTakeReady"`,
		`x-bind:disabled="isAuK"`,
		`x-bind:disabled="!isBreeze"`,
		`x-bind:disabled="!needsVoice"`,
		`x-bind:disabled="aukSourceMissing"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("conditional field contract missing %q", want)
		}
	}
}
