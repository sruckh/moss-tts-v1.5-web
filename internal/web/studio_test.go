package web

import (
	"context"
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
		{ID: 4, Kind: voices.KindCloned, Name: "Marrow", Model: "Cloned", LicenseLabel: "Cloned voice"},
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

// The poll asks for the selected take by URL, so every swap re-renders the
// highlight and the fragment that arrives asks for it again — the selection
// sustains itself across a table that is replaced every two seconds.
func TestQueuePollCarriesSelectedTake(t *testing.T) {
	selected := render(t, Queue(sampleJobs(), 0, nil, 9))
	if !strings.Contains(selected, `hx-get="/jobs?take=9"`) {
		t.Error("the queue poll does not ask for the selected take")
	}

	none := render(t, Queue(sampleJobs(), 0, nil, 0))
	if !strings.Contains(none, `hx-get="/jobs"`) {
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
	html := render(t, Compose(sampleVoices(), 1))

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

// A cloned voice card can be renamed and previewed; a stock card offers
// neither, because Rename refuses stock voices and they have no reference.
func TestVoiceCardControls(t *testing.T) {
	html := render(t, VoiceGrid(sampleVoices(), 4))

	for _, want := range []string{
		`hx-post="/voices/4/name"`,
		`src="/voices/4/reference"`,
		`aria-label="Rename Marrow"`,
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
	// A card holds buttons now, so it may not be one itself.
	if strings.Contains(html, `<button type="button" aria-pressed=`) {
		t.Error("the voice card is still a button and now nests buttons")
	}
}
