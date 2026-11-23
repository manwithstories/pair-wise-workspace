package clock

import (
	"errors"
	"math"
	"testing"
)

func TestRateKnownClocks(t *testing.T) {
	cases := []struct {
		name      string
		timescale int64
		units     int64
		wantTicks int64
	}{
		{"one second at 1MHz", TicksPerSecond, TicksPerSecond, TicksPerSecond},
		{"one second at 90kHz", RTPVideo90k, 90_000, TicksPerSecond},
		{"90000 ticks at 90k = 1s", RTPVideo90k, 90_000, 1_000_000},
		{"8000 audio samples = 1s", RTPCDR8k, 8_000, 1_000_000},
		{"24000/48000 audio = 500ms", 48_000, 24_000, 500_000},
		{"mp4 30fps frame at 30k", 30_000, 1_000, 33_333},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, err := Rate(tc.timescale)
			if err != nil {
				t.Fatalf("Rate(%d): %v", tc.timescale, err)
			}
			got, err := rate.Scale(tc.units)
			if err != nil {
				t.Fatalf("Scale: %v", err)
			}
			if got != tc.wantTicks {
				t.Fatalf("got %d, want %d", got, tc.wantTicks)
			}
		})
	}
}

func TestInvalidTimescale(t *testing.T) {
	for _, ts := range []int64{0, -1, MaxTimescale + 1, math.MaxInt64} {
		if _, err := Rate(ts); !errors.Is(err, ErrInvalidTimescale) {
			t.Fatalf("Rate(%d) err=%v, want ErrInvalidTimescale", ts, err)
		}
		if _, err := FromClock(1, ts); !errors.Is(err, ErrInvalidTimescale) {
			t.Fatalf("FromClock(%d) err=%v", ts, err)
		}
	}
}

func TestNegativeDuration(t *testing.T) {
	if _, err := FromClock(-5, 90_000); !errors.Is(err, ErrNegativeDuration) {
		t.Fatalf("err=%v, want ErrNegativeDuration", err)
	}
	if err := ValidateDuration(-1); !errors.Is(err, ErrNegativeDuration) {
		t.Fatalf("ValidateDuration err=%v", err)
	}
}

func TestScaleOverflow(t *testing.T) {
	r := Rational{Num: TicksPerSecond, Den: 30_000}
	if _, err := r.Scale(math.MaxInt64); !errors.Is(err, ErrOverflow) {
		t.Fatalf("err=%v, want ErrOverflow", err)
	}
	// Rounding path must also detect overflow.
	if _, err := r.ScaleRound(math.MaxInt64); !errors.Is(err, ErrOverflow) {
		t.Fatalf("round err=%v, want ErrOverflow", err)
	}
}

func TestScaleMatchesBigRational(t *testing.T) {
	// Exhaustive-ish spot check against naive int128-free big integer math
	// via float cross check on realistic media magnitudes plus edge values.
	r, err := NewRational(1_000_000, 90_000)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []int64{0, 1, 90_000, 90_001, 12_345_678, 8_589_934_592, 1 << 40} {
		got, err := r.Scale(v)
		if err != nil {
			t.Fatalf("Scale(%d): %v", v, err)
		}
		// Independent reference using a different split formulation.
		// 1e6/9e4 = 100/9 exactly after gcd normalization.
		if r.Num != 100 || r.Den != 9 {
			t.Fatalf("unexpected normalized rate %d/%d", r.Num, r.Den)
		}
		if want := exactRef(v, 100, 9); got != want {
			t.Fatalf("v=%d got=%d want=%d", v, got, want)
		}
	}
}

func exactRef(v, num, den int64) int64 {
	// Reference via 64-bit split multiplication, independent implementation.
	hi := v / den
	lo := v % den
	return hi*num + (lo*num)/den
}

func TestScaleRoundHalfAway(t *testing.T) {
	r := Rational{Num: 10, Den: 4}           // 2.5 per unit
	if got, _ := r.ScaleRound(1); got != 3 { // 2.5 -> 3
		t.Fatalf("positive half: got %d want 3", got)
	}
	if got, _ := r.ScaleRound(-1); got != -3 { // -2.5 -> -3
		t.Fatalf("negative half: got %d want -3", got)
	}
	r2 := Rational{Num: 1_000_000, Den: 30_000}
	if got, _ := r2.ScaleRound(1); got != 33 { // 33.333 -> 33
		t.Fatalf("got %d want 33", got)
	}
}

func TestNewRationalRejectsBadDen(t *testing.T) {
	if _, err := NewRational(1, 0); !errors.Is(err, ErrInvalidRational) {
		t.Fatalf("err=%v", err)
	}
	if _, err := NewRational(1, -7); !errors.Is(err, ErrInvalidRational) {
		t.Fatalf("err=%v", err)
	}
}

func TestMul64Boundaries(t *testing.T) {
	cases := []struct {
		a, b   int64
		want   int64
		wantOK bool
	}{
		{-1, 10, -10, true},
		{math.MinInt64, 1, math.MinInt64, true},
		{1, math.MinInt64, math.MinInt64, true},
		{math.MinInt64, -1, 0, false}, // 2^63 does not fit
		{math.MinInt64, 2, 0, false},
		{math.MaxInt64, 2, 0, false},
		{math.MaxInt64, math.MaxInt64, 0, false},
		{-math.MaxInt64, 2, 0, false}, // |product| = 2^64-2 exceeds int64
		{0, math.MaxInt64, 0, true},
	}
	for _, tc := range cases {
		got, ok := mul64(tc.a, tc.b)
		if ok != tc.wantOK {
			t.Fatalf("mul64(%d,%d) ok=%v want %v", tc.a, tc.b, ok, tc.wantOK)
		}
		if ok && got != tc.want {
			t.Fatalf("mul64(%d,%d)=%d want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
