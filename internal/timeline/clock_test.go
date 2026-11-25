package timeline

import (
	"math"
	"testing"
)

func TestUnwrap33AcrossPCRAndPTSWrap(t *testing.T) {
	var u Unwrapper
	start := uint64(mod33 - 90)
	values := []uint64{start, uint64(mod33 - 1), 0, 90, 180}
	want := []int64{mod33 - 90, mod33 - 1, mod33, mod33 + 90, mod33 + 180}
	for i, raw := range values {
		got, err := u.Unwrap33(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got != want[i] {
			t.Fatalf("case %d: got %d want %d", i, got, want[i])
		}
	}
}

func TestUnwrap33RejectsOverflow(t *testing.T) {
	var u Unwrapper
	if _, err := u.Unwrap33(1 << 33); err == nil {
		t.Fatal("expected timestamp overflow")
	}
}

func TestPCRCombineAndWrap(t *testing.T) {
	var u Unwrapper
	if _, err := u.Unwrap33(uint64(mod33 - 2)); err != nil {
		t.Fatal(err)
	}
	pcr, err := UnwrapPCR(&u, 1, 270)
	if err != nil {
		t.Fatal(err)
	}
	if pcr.Base90kHz != mod33+1 {
		t.Fatalf("unwrapped base=%d", pcr.Base90kHz)
	}
	if pcr.Ticks27MHz != (mod33+1)*300+270 {
		t.Fatalf("ticks=%d", pcr.Ticks27MHz)
	}
}

func TestClockOutlierDoesNotResetAnchor(t *testing.T) {
	c := NewClock(90000)
	// 30 fps-ish media tick increments, arrival advances proportionally.
	for i := uint32(0); i < 10; i++ {
		c.Observe(i*3000, int64(i)*33_333_333)
	}
	before := c.Estimate()
	// A huge delayed packet must be rejected rather than resetting the clock.
	d := c.Observe(30000, 10_000_000_000)
	if d.Outliers == 0 {
		t.Fatalf("expected outlier count, got %+v", d)
	}
	after := c.Estimate()
	if after.Accepted != before.Accepted {
		t.Fatalf("outlier changed accepted window: before=%d after=%d", before.Accepted, after.Accepted)
	}
	if math.Abs(after.DriftPPM) > 1 {
		t.Fatalf("nominal stream drift should stay near zero, got %.3f", after.DriftPPM)
	}
}

func TestVariableFrameDurationsAreNotRoundedToFixedFPS(t *testing.T) {
	pts := []int64{0, 3003, 6006, 10800, 13803} // mixed 3003 and 4794-ish ticks
	var got []float64
	for i := 1; i < len(pts); i++ {
		got = append(got, PTSDurationMS(pts[i]-pts[i-1]))
	}
	want := []float64{33.3666666667, 33.3666666667, 53.2666666667, 33.3666666667}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-6 {
			t.Fatalf("duration %d = %.6f want %.6f (fixed-FPS fabrication?)", i, got[i], want[i])
		}
	}
}

func TestBoundarySkew(t *testing.T) {
	if got := BoundarySkewMS(90000, 91799); got > 20 {
		t.Fatalf("1799 ticks should be <=20ms, got %v", got)
	}
	if got := BoundarySkewMS(90000, 91801); got <= 20 {
		t.Fatalf("1801 ticks should exceed 20ms, got %v", got)
	}
}
