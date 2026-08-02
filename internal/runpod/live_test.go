package runpod

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveWordTimings renders a short English phrase on the real RunPod
// endpoint and confirms the completed payload carries word_timings that decode
// through Output.WordTimings with a valid shape (start<end, monotonic). It
// exercises the same Client the poller uses, so it is an end-to-end check of
// the consumer-side decode path against a live worker.
//
// Skipped unless TIMBRE_LIVE=1 is set alongside RUNPOD_API_KEY and
// RUNPOD_ENDPOINT — run it under `infisical run` so the key lands in env (the
// endpoint lives in .env). A transient RunPod outage (5xx/network) is retried a
// few times and then reported; a permanent error (auth, schema) fails the test.
func TestLiveWordTimings(t *testing.T) {
	if os.Getenv("TIMBRE_LIVE") != "1" {
		t.Skip("set TIMBRE_LIVE=1 (and RUNPOD_API_KEY/RUNPOD_ENDPOINT) to run the live test")
	}
	endpoint := os.Getenv("RUNPOD_ENDPOINT")
	key := os.Getenv("RUNPOD_API_KEY")
	if endpoint == "" || key == "" {
		t.Skip("RUNPOD_ENDPOINT or RUNPOD_API_KEY unset — live test needs both")
	}

	client := New(endpoint, key)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Second)
	defer cancel()

	sub, err := client.Submit(ctx, Input{
		Text:     "One. Two. Three. Four.",
		Language: "English",
		Stream:   false,
	})
	if err != nil {
		t.Fatalf("submit render: %v", err)
	}

	var res StatusResult
	deadline := time.Now().Add(385 * time.Second)
	completed := false
	for !completed {
		if time.Now().After(deadline) {
			t.Fatalf("render %s did not complete before the deadline (last status %q)", sub.ID, res.Status)
		}
		res, err = client.Status(ctx, sub.ID)
		if err != nil {
			if IsPermanent(err) {
				t.Fatalf("status %s: permanent error: %v", sub.ID, err)
			}
			time.Sleep(2 * time.Second)
			continue
		}
		switch res.Status {
		case StatusCompleted:
			completed = true
		case StatusFailed:
			t.Fatalf("render %s failed at RunPod: %s", sub.ID, res.ErrorString())
		default:
			time.Sleep(2 * time.Second)
		}
	}

	if res.Output.AudioBase64 == "" {
		t.Fatalf("render %s completed with no audio", sub.ID)
	}
	if res.Output.WordTimings == nil {
		t.Fatalf("render %s completed without word_timings — alignment was omitted", sub.ID)
	}
	wt := res.Output.WordTimings
	if wt.Source == "" {
		t.Errorf("word_timings.source is empty")
	}
	if len(wt.Words) < 2 {
		t.Fatalf("word_timings has %d words, want >= 2", len(wt.Words))
	}
	var prev float64
	for i, w := range wt.Words {
		if w.W == "" {
			t.Errorf("word %d has empty text", i)
		}
		if !(w.Start < w.End) {
			t.Errorf("word %d (%q) start=%v end=%v, want start<end", i, w.W, w.Start, w.End)
		}
		if w.Start < prev {
			t.Errorf("word %d start=%v, want >= previous start %v (monotonic)", i, w.Start, prev)
		}
		prev = w.Start
	}
	t.Logf("live word_timings OK: source=%s frame_rate=%v words=%d first=%+v",
		wt.Source, wt.FrameRate, len(wt.Words), wt.Words[0])
}
