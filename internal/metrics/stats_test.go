package metrics_test

import (
	"testing"

	"mediacheck/internal/metrics"
	"mediacheck/internal/mux"
)

func TestBitrateAndDeviation(t *testing.T) {
	// 125000 bytes over 90000 ticks (1 second) is 1 Mbps.
	if got := metrics.Bitrate(125000, 90000); got != 1_000_000 {
		t.Fatalf("bitrate=%d", got)
	}
	if got := metrics.DeviationPercent(1_000_000, 1_050_000); got != 5 {
		t.Fatalf("deviation=%v", got)
	}
}

func TestCheckSegmentsTolerance(t *testing.T) {
	segments := []mux.SegmentInfo{
		{BasePTS: 0, DurationTicks: 90000, Bytes: 124866},
		{BasePTS: 90000, DurationTicks: 90000, Bytes: 125100},
	}
	reports, err := metrics.CheckSegments(1_000_000, segments)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reports {
		if !r.WithinTolerance {
			t.Fatalf("report %+v outside tolerance", r)
		}
	}

	bad := []mux.SegmentInfo{{DurationTicks: 90000, Bytes: 200000}}
	if _, err := metrics.CheckSegments(1_000_000, bad); err == nil {
		t.Fatal("expected bitrate tolerance failure")
	}
}
