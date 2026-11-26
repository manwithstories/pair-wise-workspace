package report

import (
	"testing"
	"time"
)

func TestRecoveryPercent(t *testing.T) {
	if got := RecoveryPercent(0, 0); got != 100.0 {
		t.Fatalf("no bad blocks = %f, want 100", got)
	}
	if got := RecoveryPercent(3, 1); got != 75.0 {
		t.Fatalf("3/4 recovered = %f, want 75", got)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	d := []time.Duration{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := Percentile(d, 95); got != 100 {
		t.Fatalf("P95 = %v, want 100", got)
	}
	if got := Percentile(d, 50); got != 50 {
		t.Fatalf("P50 = %v, want 50", got)
	}
	if got := Percentile(nil, 95); got != 0 {
		t.Fatalf("empty percentile = %v, want 0", got)
	}
}

func TestRenderTextContainsRequiredFields(t *testing.T) {
	s := &Summary{
		AVSyncP95US: 12, LossRecoveryPct: 100.0, PipelineP95MS: 0.5,
		Segments: 7, SilenceFrames: 99, DedupHits: 3,
		UnrecoveredBlocks: []int{5, 2}, RecoveredBlocks: []int{1},
		Degraded: true, TrackCount: 2,
	}
	out := s.RenderText()
	for _, key := range []string{
		"av_sync_p95_us", "loss_recovery_pct", "pipeline_p95_ms",
		"segments", "silence_frames", "dedup_hits", "unrecovered_blocks",
	} {
		if !contains(out, key+"=") {
			t.Fatalf("rendered summary missing field %s:\n%s", key, out)
		}
	}
	if !contains(out, "unrecovered_blocks=2,5") {
		t.Fatal("unrecovered blocks must render sorted")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
