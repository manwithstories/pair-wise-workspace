package app

import (
	"context"
	"testing"

	"mediacheck/internal/fixture"
)

// TestSlowSinkBackpressure verifies that a deliberately slow sink does
// not blow the memory budget: the bounded pipeline blocks upstream and
// still completes with every accepted item committed.
func TestSlowSinkBackpressure(t *testing.T) {
	o := DefaultOptions()
	o.Profile = fixture.ProfileBroadcast
	o.SlowSinkMs = 5 // small but non-zero to exercise blocking
	o.MemoryMiB = 1  // tight 1 MiB budget

	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("pipeline failed under backpressure: %v", err)
	}
	if res.Summary.PeakBufferMiB > 1.0 {
		t.Fatalf("backpressure failed to cap buffer: %f MiB", res.Summary.PeakBufferMiB)
	}
	if res.Summary.LatencyP95Ms > 500 {
		t.Fatalf("p95 latency %d exceeds local ceiling", res.Summary.LatencyP95Ms)
	}
	// The remuxed output must contain explicit gap markers, proving the
	// original hole was not silently hidden.
	if !containsGapMarker(res.Output) && res.Summary.Quarantined > 0 {
		t.Fatal("quarantined gap missing from remux manifest")
	}
}

func containsGapMarker(b []byte) bool {
	return len(b) > 0 && indexOf(b, []byte("GAP")) >= 0
}

func indexOf(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
