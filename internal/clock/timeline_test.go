package clock

import (
	"errors"
	"math"
	"testing"
)

func TestConvertMapsToNanos(t *testing.T) {
	c, err := NewTrackClock("video", 90_000, 1<<32)
	if err != nil {
		t.Fatalf("new clock: %v", err)
	}
	// 90000 ticks == 1 second == 1e9 ns.
	r := c.Convert(90_000)
	if r.NS != 0 {
		t.Fatalf("first sample should anchor origin, got %d", r.NS)
	}
	if r.Wrapped || r.Duplicate || r.Regressed {
		t.Fatalf("first sample flagged: %+v", r)
	}
	r = c.Convert(180_000)
	if r.NS != 1e9 {
		t.Fatalf("second sample = %d, want %d", r.NS, int64(1e9))
	}
	// Non-integral tick conversions must be exact integer arithmetic.
	c2, _ := NewTrackClock("audio", 48_000, 1<<33)
	c2.Convert(43_200)
	if x := c2.Convert(48_200).NS; x != 104_166_666 {
		t.Fatalf("unexpected mapping: %d (want 104166666)", x)
	}
}

func TestWrapDetected(t *testing.T) {
	c, _ := NewTrackClock("rtp", 90_000, 1<<32)
	c.Convert(1<<32 - 9000)
	c.Convert(1<<32 - 4500)
	prev := c.LastNS()
	r := c.Convert(4500) // wrapped past zero
	if !r.Wrapped {
		t.Fatalf("expected wrap flag, got %+v", r)
	}
	if r.NS <= prev {
		t.Fatalf("axis must stay monotonic across wrap: %d <= %d", r.NS, prev)
	}
	if r.Regressed {
		t.Fatalf("wrap must not be reported as regression")
	}
	c.Convert(9000)
	if r := c.Convert(9000); !r.Duplicate || !r.Regressed {
		t.Fatalf("duplicate not flagged: %+v", r)
	}
	if c.WrapCount != 1 || c.DupCount != 1 {
		t.Fatalf("counters wraps=%d dups=%d", c.WrapCount, c.DupCount)
	}
}

func TestRegressionWithoutWrap(t *testing.T) {
	c, _ := NewTrackClock("v", 1000, 0) // no modulus: no wraps possible
	c.Convert(100)
	r := c.Convert(90)
	if !r.Regressed {
		t.Fatalf("regression not detected: %+v", r)
	}
	if c.RegCount != 1 {
		t.Fatalf("reg count = %d", c.RegCount)
	}
}

func TestInvalidTimescaleRejected(t *testing.T) {
	_, err := NewTrackClock("bad", 0, 0)
	if !IsKind(err, KindInvalid) {
		t.Fatalf("want invalid kind, got %v", err)
	}
}

func TestFitDriftRecoversSlope(t *testing.T) {
	// Media clock runs 500 ppm fast relative to reference.
	const ppm = 500
	pts := make([]DriftPoint, 0, 10)
	for i := 0; i < 10; i++ {
		ref := int64(i) * 100_000_000
		media := ref + ref/1000*ppm/1000
		pts = append(pts, DriftPoint{RefNS: ref, MediaNS: media})
	}
	f, err := FitDrift(pts, DefaultFitConfig())
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	want := int64(nanosPerSecond) + ppm*1000 // 1.0005 x real time
	if f.SlopePPB < want-2000 || f.SlopePPB > want+2000 {
		t.Fatalf("slope = %d ppb, want ~%d", f.SlopePPB, want)
	}
	pred := f.OffsetNS + scaleSlope(f.SlopePPB, pts[9].RefNS)
	if d := pred - pts[9].MediaNS; d < -1000 || d > 1000 {
		t.Fatalf("prediction off by %d", d)
	}
}

func TestFitDriftAmbiguous(t *testing.T) {
	cases := map[string][]DriftPoint{
		"short baseline": {{0, 0}, {10, 0}, {20, 0}, {30, 0}},
		"dup ref":        {{0, 0}, {1e9, 1e9}, {1e9, 1e9 + 5}, {2e9, 2e9}},
		"too few":        {{0, 0}, {1e9, 1e9}, {2e9, 2e9}},
	}
	for name, pts := range cases {
		_, err := FitDrift(pts, DefaultFitConfig())
		if !IsKind(err, KindUnrecoverable) {
			t.Fatalf("%s: want unrecoverable, got %v", name, err)
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeDriftAmbiguous {
			t.Fatalf("%s: wrong code %v", name, err)
		}
	}
}

func TestFitDriftRejectsBadResidual(t *testing.T) {
	pts := []DriftPoint{{0, 0}, {1e9, 1e9}, {2e9, 2e9}, {3e9, 9e9}}
	_, err := FitDrift(pts, DefaultFitConfig())
	if !IsKind(err, KindUnrecoverable) {
		t.Fatalf("want unrecoverable, got %v", err)
	}
}

func TestConvertReportsOverflowInsteadOfWrapping(t *testing.T) {
	// A 1kHz clock driven to the very top of the 64-bit counter space must
	// not produce a wrapped, negative axis position.
	c, err := NewTrackClock("v", 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.Convert(1)
	r := c.Convert(math.MaxUint64)
	if !r.Regressed {
		t.Fatalf("overflow not reported: %+v", r)
	}
	if r.NS != 0 {
		t.Fatalf("overflow produced a non-zero timestamp: %d", r.NS)
	}
	if c.RegCount != 1 {
		t.Fatalf("overflow not counted: %d", c.RegCount)
	}
}
