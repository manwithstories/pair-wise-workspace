package model

import (
	"strings"
	"testing"
)

func TestClockCalibrateNoDrift(t *testing.T) {
	c := Clock{Timescale: 90_000}
	if got := c.Calibrate(90_000); got != Instant(1_000_000_000) {
		t.Fatalf("calibrate 90k ticks = %v, want 1s", got)
	}
	if got := c.Calibrate(0); got != 0 {
		t.Fatalf("zero ticks = %v", got)
	}
}

func TestClockCalibrateDriftMonotone(t *testing.T) {
	c := Clock{Timescale: 90_000, DriftPPM: 85}
	prev := Instant(-1)
	for tick := int64(0); tick <= 90_000*60; tick += 37 {
		v := c.Calibrate(tick)
		if v < prev {
			t.Fatalf("non-monotone at tick %d: %v < %v", tick, v, prev)
		}
		prev = v
	}
	// A fast source clock maps 1s of source ticks to slightly less than 1s.
	if got := c.Calibrate(90_000); got >= Instant(1_000_000_000) {
		t.Fatalf("drift did not compress time: %v", got)
	}
}

func TestUnwrap32Wrap(t *testing.T) {
	// Values straddling the 32-bit wrap point with one reordered sample.
	raw := []uint32{4_294_967_000, 100, 4_294_966_900, 200, 50}
	got := Unwrap32(raw)
	want := []int64{4_294_967_000, 4_294_967_396, 4_294_966_900, 4_294_967_496, 4_294_967_346}
	if len(got) != len(want) {
		t.Fatalf("len=%d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("i=%d got=%d want=%d", i, got[i], want[i])
		}
	}
}

func TestCadenceExactAndDriftFree(t *testing.T) {
	rates := []Rational{{24, 1}, {25, 1}, {23, 1}}
	c, err := NewCadence(rates)
	if err != nil {
		t.Fatal(err)
	}
	ts := int64(1_000_000)
	prev := int64(-1)
	for f := int64(0); f < 720; f++ {
		s := c.Start(f, ts)
		if s <= prev {
			t.Fatalf("frame %d not increasing: %d <= %d", f, s, prev)
		}
		prev = s
	}
	// One full cycle = 1/24 + 1/25 + 1/23 seconds.
	cycle := c.Start(3, ts)
	wantApprox := int64(float64(ts) * (1.0/24 + 1.0/25 + 1.0/23))
	if diff := cycle - wantApprox; diff > 60 || diff < -60 {
		t.Fatalf("cycle length %d, want ~%d", cycle, wantApprox)
	}
	// Two identical runs must be identical (no float accumulation).
	for f := int64(0); f < 100; f++ {
		if c.Start(f, ts) != c.Start(f, ts) {
			t.Fatalf("non-deterministic at %d", f)
		}
	}
}

func TestCadenceRejectsBadRate(t *testing.T) {
	if _, err := NewCadence(nil); err == nil {
		t.Fatal("empty cadence accepted")
	}
	if _, err := NewCadence([]Rational{{0, 1}}); err == nil {
		t.Fatal("zero rate accepted")
	}
}

func TestErrorExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{NewError(CodeKLVLength, "bad"), 2},
		{NewError(CodeUsage, "bad"), 1},
		{NewError(CodeInternal, "bad"), 1},
	}
	for _, tc := range cases {
		if got := ExitCode(tc.err); got != tc.want {
			t.Fatalf("ExitCode(%v)=%d want %d", tc.err, got, tc.want)
		}
	}
	e := NewError(CodeMP4BoxLength, "x").WithTrack("v1").WithOffset(42)
	s := e.Error()
	for _, frag := range []string{"ERR_MP4_BOX_LENGTH", "track=v1", "offset=42"} {
		if !strings.Contains(s, frag) {
			t.Fatalf("error %q missing %q", s, frag)
		}
	}
}

func TestFingerprintOrderIndependentOfSampleIndexOnly(t *testing.T) {
	mk := func(h byte) [32]byte {
		var b [32]byte
		b[0] = h
		return b
	}
	a := &Track{Kind: KindVideo, Timescale: 1000, SampleDesc: "avc1", Samples: []Sample{{Hash: mk(1)}, {Hash: mk(2)}}}
	b := &Track{Kind: KindVideo, Timescale: 1000, SampleDesc: "avc1", Samples: []Sample{{Hash: mk(1)}, {Hash: mk(2)}}}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("identical tracks differ in fingerprint")
	}
	c := &Track{Kind: KindVideo, Timescale: 1000, SampleDesc: "avc1", Samples: []Sample{{Hash: mk(2)}, {Hash: mk(1)}}}
	if a.Fingerprint() == c.Fingerprint() {
		t.Fatal("sample-order change must change fingerprint")
	}
}

func TestCropValidate(t *testing.T) {
	ok := Crop{Valid: true, X: 10, Y: 10, W: 100, H: 100}
	if err := ok.Validate(200, 200); err != nil {
		t.Fatal(err)
	}
	bad := Crop{Valid: true, X: 150, Y: 10, W: 100, H: 100}
	if err := bad.Validate(200, 200); err == nil {
		t.Fatal("out-of-range crop accepted")
	}
}
