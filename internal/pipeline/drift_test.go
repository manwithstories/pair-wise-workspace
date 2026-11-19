package pipeline

import (
	"testing"

	"archive.local/recover/internal/testkit"
)

// TestDriftBreakNotExtrapolated locks the rule that no fit predicts across a
// clock discontinuity: when a source clock jumps mid-capture, every sample
// between the jump and the next trusted anchor must still be within the
// configured sync budget.
func TestDriftBreakNotExtrapolated(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Case = testkit.CaseWrapVFRDrift
	cfg.DurationNs = 60_000_000_000
	cfg.MaxAlignNs = 40_000_000
	res, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A break has to have been detected and reported; if the implementation
	// ever stops segmenting, this test fails rather than silently accepting a
	// larger sync error.
	found := false
	for _, i := range res.Report.Issues {
		if i.Code == clockCodeDriftBreak {
			found = true
		}
	}
	if !found {
		t.Fatalf("no drift break reported: %v", res.Report.SortedIssueCodes())
	}
	if got := res.Report.Metrics.SyncErrorMs; got > 20 {
		t.Fatalf("sync error %.3f ms across a break", got)
	}
}

const clockCodeDriftBreak = "clock.drift_break"
