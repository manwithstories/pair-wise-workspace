package fixture

import "testing"

// TestSegmentCountAndDuration locks the 1-second segmentation used to satisfy
// the segment_p95_ms <= 1000 acceptance bound.
func TestSegmentCountAndDuration(t *testing.T) {
	if got := segmentCount(45, 1); got != 45 {
		t.Fatalf("45s at 1s segments = %d, want 45", got)
	}
	plan := BuildPlan(ProfileClean, CameraNames(3), 45)
	if plan.SegmentSec != 1.0 {
		t.Fatalf("segment sec = %v, want 1.0", plan.SegmentSec)
	}
}
