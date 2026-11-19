package clock_test

import (
	"testing"

	"archivearchivist/internal/clock"
	"archivearchivist/internal/model"
)

// TestUnwrapStaysLinearAcrossManyWraps checks that repeated modulus crossings
// accumulate exactly and that normalized time never jumps backwards.
func TestUnwrapStaysLinearAcrossManyWraps(t *testing.T) {
	mod := int64(1 << 16)
	start := mod - 300
	samples := make([]clock.Sample, 0, 400)
	for i := 0; i < 400; i++ {
		ext := start + int64(i)*7
		samples = append(samples, clock.Sample{Seq: uint32(i + 1), Orig: ext % mod})
	}
	res := clock.Unwrap(samples, clock.Options{Modulus: mod})
	wantWraps := int((start+399*7)/mod) - int(start/mod)
	if res.Wraps != wantWraps {
		t.Fatalf("wraps = %d, want %d", res.Wraps, wantWraps)
	}
	for i := 1; i < len(res.Points); i++ {
		if got := res.Points[i].Norm - res.Points[i-1].Norm; got != 7 {
			t.Fatalf("step %d = %d ticks, want 7", i, got)
		}
	}
}

// TestLatePacketAfterMultipleWraps verifies a delayed packet from an earlier
// epoch does not corrupt the epoch counter.
func TestLatePacketAfterMultipleWraps(t *testing.T) {
	mod := int64(1 << 16)
	seq := []int64{mod - 10, 5, mod + 5, 2*mod - 3}
	samples := make([]clock.Sample, 0, len(seq)+1)
	for i, v := range seq {
		samples = append(samples, clock.Sample{Seq: uint32(i + 1), Orig: v % mod})
	}
	// A packet from the first epoch arrives last.
	samples = append(samples, clock.Sample{Seq: 99, Orig: (mod - 5) % mod})
	res := clock.Unwrap(samples, clock.Options{Modulus: mod})
	last := res.Points[len(res.Points)-1]
	if last.Norm >= res.Points[0].Norm+2*mod {
		t.Fatalf("late packet normalized into a later epoch: %d", last.Norm)
	}
	if last.Norm < res.Points[0].Norm {
		t.Fatalf("late packet normalized before the stream start: %d", last.Norm)
	}
}

// TestSequenceOrderIndependence confirms unwrapping depends only on the
// timestamp sequence given, not on any hidden global state.
func TestSequenceOrderIndependence(t *testing.T) {
	a := []clock.Sample{{Seq: 1, Orig: 100}, {Seq: 2, Orig: 200}}
	b := []clock.Sample{{Seq: 1, Orig: 9000}, {Seq: 2, Orig: 100}}
	ra := clock.Unwrap(a, clock.Options{Modulus: 10000})
	rb := clock.Unwrap(b, clock.Options{Modulus: 10000})
	if ra.Wraps != 0 {
		t.Fatalf("unexpected wrap in monotonic stream")
	}
	if rb.Wraps != 1 {
		t.Fatalf("wrap = %d, want 1", rb.Wraps)
	}
	if model.FromTicks(rb.Points[1].Norm-rb.Points[0].Norm, 10000).Sign() < 0 {
		t.Fatalf("wrapped stream moved backwards")
	}
}
