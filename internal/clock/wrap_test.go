package clock

import "testing"

func TestUnwrap33BitWrap(t *testing.T) {
	const mod = uint64(1) << 33
	u := NewUnwrapper(mod)
	vals := []uint64{mod - 10, mod - 5, 0, 5, 10}
	want := []int64{int64(mod) - 10, int64(mod) - 5, int64(mod), int64(mod) + 5, int64(mod) + 10}
	for i, v := range vals {
		if got := u.Unwrap(v); got != want[i] {
			t.Fatalf("step %d: got %d want %d", i, got, want[i])
		}
	}
	if u.Wraps != 1 {
		t.Fatalf("wraps=%d", u.Wraps)
	}
}

func TestUnwrapJitterDoesNotRewind(t *testing.T) {
	u := NewUnwrapper(1 << 33)
	u.Unwrap(1000)
	if got := u.Unwrap(900); got != 900 {
		t.Fatalf("jitter produced %d", got)
	}
	if got := u.Unwrap(1100); got != 1100 {
		t.Fatalf("resume produced %d", got)
	}
	if u.Wraps != 0 {
		t.Fatalf("wraps=%d", u.Wraps)
	}
}

func TestUnwrapDisabled(t *testing.T) {
	u := NewUnwrapper(0)
	if got := u.Unwrap(42); got != 42 {
		t.Fatalf("got %d", got)
	}
}

func TestDriftEstimatorPPM(t *testing.T) {
	var d DriftEstimator
	d.Observe(0, 0)
	d.Observe(1_000_000, 1_000_120)
	if ppm := d.PPM(); ppm < 119.9 || ppm > 120.1 {
		t.Fatalf("ppm=%f", ppm)
	}
}

func TestDriftEstimatorNeedsTwoSamples(t *testing.T) {
	var d DriftEstimator
	d.Observe(5, 7)
	if d.PPM() != 0 {
		t.Fatalf("ppm=%f", d.PPM())
	}
}
