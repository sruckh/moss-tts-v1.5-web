package web

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/voices"
)

// The ten palette hexes (DESIGN.md §1.1), lowercased for case-insensitive
// comparison. Nothing else may appear in a generated asset — derived values
// exist only as color-mix() between two of these, never as a new hex.
var paletteHexes = map[string]bool{
	"#0000ff": true, // Blue — decorative only
	"#83008c": true, // Purple
	"#c95d00": true, // Burnt
	"#006795": true, // Teal
	"#9eb600": true, // Olive
	"#c98500": true, // Amber
	"#009044": true, // Green
	"#d5d5f4": true, // Paper
	"#4a4ac8": true, // Indigo
	"#00003e": true, // Ink
}

var (
	hexPattern = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	// Tailwind emits @property defaults (#0000, #fff) and --tw-* custom
	// property declarations for shadow/ring utilities this design never
	// uses. They are framework boilerplate, not authored color — stripped
	// before the scan (see AGENTS.md, "CSS").
	propertyPattern = regexp.MustCompile(`(?s)@property\s+--[^{]*\{[^{]*\}`)
	twDeclPattern   = regexp.MustCompile(`--tw-[a-z0-9-]+\s*:[^;}]+;`)
)

// foreignHexes returns every distinct hex in content that is not one of the
// ten palette colors, after stripping Tailwind's --tw-* boilerplate.
func foreignHexes(content string) []string {
	cleaned := propertyPattern.ReplaceAllString(content, "")
	cleaned = twDeclPattern.ReplaceAllString(cleaned, "")
	seen := map[string]bool{}
	var bad []string
	for _, h := range hexPattern.FindAllString(cleaned, -1) {
		l := strings.ToLower(h)
		if paletteHexes[l] || seen[l] {
			continue
		}
		seen[l] = true
		bad = append(bad, h)
	}
	return bad
}

// The compiled stylesheet is the design system's enforcement point: if any
// color outside the ten reaches app.css, the build is wrong no matter how the
// markup looks.
func TestPaletteExhaustiveCompiledCSS(t *testing.T) {
	data, err := os.ReadFile("app.css")
	if err != nil {
		t.Fatalf("read app.css: %v (did the Tailwind build step run?)", err)
	}
	if bad := foreignHexes(string(data)); len(bad) > 0 {
		t.Errorf("app.css contains colors outside the 10 palette hexes: %v", bad)
	}
}

// The rendered HTML gets the same check: inline styles and attributes must
// stay inside the palette too.
func TestPaletteExhaustiveRenderedHTML(t *testing.T) {
	vs := []voices.Voice{
		{ID: 1, Kind: voices.KindStock, Name: "Moss", Model: "MOSS-TTS v1.5", LicenseLabel: "OpenMOSS Community"},
		{ID: 2, Kind: voices.KindStock, Name: "Legacy", Model: "MOSS-TTS v1.5", LicenseLabel: "Apache-2.0"},
		{ID: 4, Kind: voices.KindCloned, Name: "Marrow", Model: "Cloned", LicenseLabel: "Cloned voice"},
	}
	items := []jobs.Job{
		{ID: 9, UserID: 1, Status: jobs.StatusReady, VoiceID: 1, VoiceName: "Moss", VoiceKind: voices.KindStock,
			Text: "Welcome back. Your last render finished.", AudioPath: "/x.wav", Format: "wav",
			SampleRate: 24000, ExecMS: 4100, Model: jobs.DefaultModel},
		{ID: 8, UserID: 1, Status: jobs.StatusInProgress, VoiceID: 2, VoiceName: "Legacy", VoiceKind: voices.KindStock,
			Text: "Chapter two, full read.", ExecMS: 38000},
		{ID: 7, UserID: 1, Status: jobs.StatusQueued, VoiceID: 4, VoiceName: "Marrow", VoiceKind: voices.KindCloned,
			Text: "Pronunciation test."},
		{ID: 6, UserID: 1, Status: jobs.StatusFailed, VoiceID: 1, VoiceName: "Moss", VoiceKind: voices.KindStock,
			Text: "Outro.", Error: "endpoint rejected the submission", ExecMS: 2400},
	}
	durations := map[int64]string{9: "0:06.02"}
	admin := AdminData{
		Users: []AdminUser{
			{ID: 1, Username: "admin", Email: "admin@example.com", Role: auth.RoleAdmin, Status: auth.StatusApproved},
			{ID: 2, Username: "reader", Role: auth.RoleUser, Status: auth.StatusDisabled},
		},
		Requests: []auth.AccessRequest{
			{ID: 3, Username: "applicant", Email: "hello@example.com", Status: auth.RequestPending, CreatedAt: "2026-08-07"},
		},
		Voices: []AdminVoice{
			{ID: 1, Kind: voices.KindStock, Name: "Moss", Model: "MOSS-TTS v1.5", IsGlobal: true},
			{ID: 4, Kind: voices.KindCloned, Name: "Marrow", Model: "Cloned", OwnerID: 2, OwnerName: "reader"},
		},
	}

	pages := map[string]templ.Component{
		"Studio":          Studio(items, vs, durations, 9),
		"QueuePage":       QueuePage(items, vs, durations, 0),
		"Queue":           Queue(items, 9, durations, 9),
		"PlayerReady":     PlayerBody(items[0], durations[9]),
		"PlayerBusy":      PlayerBody(items[1], ""),
		"PlayerFailed":    PlayerBody(items[3], ""),
		"PlayerEmpty":     PlayerBody(jobs.Job{}, ""),
		"VoiceLibrary":    VoiceLibrary(vs),
		"VoiceGrid":       VoiceGrid(vs, 4),
		"Admin":           AdminPage(admin),
		"Login":           Login("bad credentials"),
		"HoldingPending":  Holding(false),
		"HoldingDisabled": Holding(true),

		"Apply":          Apply(ApplyForm{}, ""),
		"ApplyRejected":  Apply(ApplyForm{Username: "applicant", Email: "hello@example.com"}, "that username is taken"),
		"ApplySubmitted": ApplySubmitted("applicant"),

		// Each lookup state paints a different alert variant, so all four are
		// rendered alongside the not-yet-searched page.
		"ApplyStatusBlank":    ApplyStatus("", ""),
		"ApplyStatusPending":  ApplyStatus("applicant", auth.RequestPending),
		"ApplyStatusApproved": ApplyStatus("applicant", auth.RequestApproved),
		"ApplyStatusDenied":   ApplyStatus("applicant", auth.RequestDenied),
		"ApplyStatusNone":     ApplyStatus("stranger", ApplyStateNone),
	}
	for name, page := range pages {
		var sb strings.Builder
		if err := page.Render(context.Background(), &sb); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		if bad := foreignHexes(sb.String()); len(bad) > 0 {
			t.Errorf("%s HTML contains colors outside the 10 palette hexes: %v", name, bad)
		}
	}

	// The Admin nav link and its pending badge only render for an administrator
	// with requests waiting, so they need a nav context to appear at all.
	var badged strings.Builder
	navCtx := WithNav(context.Background(), NavState{IsAdmin: true, PendingRequests: 3})
	if err := AdminPage(admin).Render(navCtx, &badged); err != nil {
		t.Fatalf("render Admin with the nav badge: %v", err)
	}
	if bad := foreignHexes(badged.String()); len(bad) > 0 {
		t.Errorf("Admin nav badge HTML contains colors outside the 10 palette hexes: %v", bad)
	}
}
