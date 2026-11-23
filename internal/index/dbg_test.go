package index

import "testing"

// TestHitRateCoverage pins the seek-coverage semantics: a request anywhere
// inside the movie duration resolves to a keyframe, and one past the end
// does not.
func TestHitRateCoverage(t *testing.T) {
	idx, err := BuildFile(buildTestMP4(t, 1200))
	if err != nil {
		t.Fatal(err)
	}
	if idx.DurationNS != 1200*33_333_333 {
		t.Logf("duration = %d", idx.DurationNS)
	}
	var req []int64
	for x := int64(40_000_000); x < idx.DurationNS; x += 160_000_000 {
		req = append(req, x)
	}
	if rate := idx.HitRate(req, 1_000_000); rate != 1.0 {
		t.Fatalf("coverage = %v, want 1.0", rate)
	}
	if rate := idx.HitRate([]int64{-1, idx.DurationNS + 1}, 0); rate != 0 {
		t.Fatalf("out-of-range resolved: %v", rate)
	}
}
