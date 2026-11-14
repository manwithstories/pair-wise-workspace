package report

import (
	"strings"
	"testing"
)

func TestEvaluateExitCodes(t *testing.T) {
	base := &Report{
		TargetBps: 1000, ActualBps: 1000, DeviationPct: 1.0,
		PSNRBySegment: []float64{38, 39}, PSNRSpreadDB: 1.0,
	}
	if code, _ := base.Evaluate(30, 2, 8); code != ExitOK {
		t.Fatalf("expected 0, got %d", code)
	}
	if code, reasons := base.Evaluate(38.5, 2, 8); code != ExitQuality {
		t.Fatalf("floor violation expected 4, got %d (%v)", code, reasons)
	}
	if code, _ := base.Evaluate(30, 0.5, 8); code != ExitQuality {
		t.Fatalf("spread violation expected 4, got %d", code)
	}
	bad := *base
	bad.DeviationPct = 12.0
	if code, _ := bad.Evaluate(0, 0, 8); code != ExitBitrate {
		t.Fatalf("deviation violation expected 3, got %d", code)
	}
	// quality takes precedence over bitrate
	bad2 := *base
	bad2.DeviationPct = 12.0
	bad2.PSNRSpreadDB = 5.0
	if code, _ := bad2.Evaluate(0, 2, 8); code != ExitQuality {
		t.Fatalf("quality should take precedence, got %d", code)
	}
}

func TestWriteJSON(t *testing.T) {
	r := &Report{TargetBps: 1200000, ActualBps: 1187291, Segments: 2,
		BitrateBySegment: []int64{1, 2}, PSNRBySegment: []float64{38.1, 37.9}}
	var sb strings.Builder
	if err := r.WriteJSON(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, key := range []string{"target_bps", "actual_bps", "deviation_pct", "segments", "psnr_mean_db", "psnr_std_db", "bitrate_by_segment"} {
		if !strings.Contains(out, key) {
			t.Fatalf("JSON missing key %q", key)
		}
	}
}

func TestSortedSegmentsStable(t *testing.T) {
	r := &Report{BitrateBySegment: []int64{9, 1, 5}}
	got := r.SortedSegments()
	for i, want := range []int{1, 2, 3} {
		if got[i] != want {
			t.Fatalf("segment order not stable: %v", got)
		}
	}
}
