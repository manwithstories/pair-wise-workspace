package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestPercentile(t *testing.T) {
	ds := []float64{10, 1, 5, 3, 7, 9, 2, 4, 6, 8}
	if got := Percentile(ds, 50); got != 6 {
		t.Fatalf("p50=%v, want 6", got)
	}
	if got := Percentile(ds, 95); got != 10 {
		t.Fatalf("p95=%v, want 10", got)
	}
	if got := Percentile(nil, 95); got != 0 {
		t.Fatalf("empty p95=%v", got)
	}
}

func TestPass(t *testing.T) {
	ok := &Report{
		SyncMaxMs: 15, LossRecoveryPct: 100, P95SegmentMs: 12,
		RollbackOK: true, RepeatHashEqual: true, Frames: 120,
	}
	if !ok.Pass() {
		t.Fatal("should pass")
	}
	cases := []Report{
		{SyncMaxMs: 21, LossRecoveryPct: 100, P95SegmentMs: 12, RollbackOK: true, RepeatHashEqual: true, Frames: 120},
		{SyncMaxMs: 15, LossRecoveryPct: 89, P95SegmentMs: 12, RollbackOK: true, RepeatHashEqual: true, Frames: 120},
		{SyncMaxMs: 15, LossRecoveryPct: 100, P95SegmentMs: 251, RollbackOK: true, RepeatHashEqual: true, Frames: 120},
		{SyncMaxMs: 15, LossRecoveryPct: 100, P95SegmentMs: 12, RollbackOK: false, RepeatHashEqual: true, Frames: 120},
		{SyncMaxMs: 15, LossRecoveryPct: 100, P95SegmentMs: 12, RollbackOK: true, RepeatHashEqual: false, Frames: 120},
		{SyncMaxMs: 15, LossRecoveryPct: 100, P95SegmentMs: 12, RollbackOK: true, RepeatHashEqual: true, Frames: 119},
	}
	for i, c := range cases {
		if c.Pass() {
			t.Fatalf("case %d should fail", i)
		}
	}
}

func TestPrint(t *testing.T) {
	r := &Report{
		SyncMaxMs: 15.5, LossRecoveryPct: 100, P95SegmentMs: 1.5,
		RollbackOK: true, RepeatHashEqual: true, Frames: 120,
		SHA256:       strings.Repeat("ab", 32),
		StageCommits: map[string]int{"parse": 1, "recover": 1, "mix": 1, "render": 1},
		HasFirstErr:  true, FirstErrSource: "rep:v1", FirstErrOffset: 4096,
	}
	var buf bytes.Buffer
	r.Print(&buf)
	out := buf.String()
	for _, want := range []string{
		"sync_max_ms=15.50", "loss_recovery_pct=100.0", "p95_segment_ms=1.50",
		"rollback_ok=true", "repeat_hash_equal=true", "frames=120",
		"sha256=" + strings.Repeat("ab", 32),
		"parse=1", "recover=1", "mix=1", "render=1",
		"first_error source=rep:v1 offset=4096",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}
