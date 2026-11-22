package clock

import (
	"errors"
	"testing"
	"time"
)

func TestTicksConversions(t *testing.T) {
	cases := []struct {
		d   time.Duration
		exp Ticks
	}{
		{time.Second, 90000},
		{time.Millisecond, 90},
		{0, 0},
		{40 * time.Millisecond, 3600},
	}
	for _, c := range cases {
		got := FromDuration(c.d)
		if got != c.exp {
			t.Fatalf("FromDuration(%v) = %d, want %d", c.d, got, c.exp)
		}
		if got.Duration() != c.d {
			t.Fatalf("round trip %v -> %d -> %v", c.d, got, got.Duration())
		}
	}
	if FromDuration(time.Second + 5*time.Microsecond).Milliseconds() != 1000 {
		t.Fatal("millisecond rounding wrong")
	}
}

func TestUnwrap32(t *testing.T) {
	// Steady increase.
	base := Ticks(1000)
	got := Unwrap32(base, 1010)
	if got != 1010 {
		t.Fatalf("plain delta = %d, want 1010", got)
	}
	// Wrap around the 32-bit modulus.
	base = Ticks(Max32 - 5)
	got = Unwrap32(base, uint32(7))
	if got != Ticks(Max32+7) {
		t.Fatalf("wrap = %d, want %d", got, Max32+7)
	}
	// Small genuine backward jump must stay negative (detectable).
	base = Ticks(5000)
	if d := Delta32(base, 4900); d != -100 {
		t.Fatalf("backward delta = %d, want -100", d)
	}
	// Large negative-looking delta is a wrap, not a regression.
	base = Ticks(10)
	if d := Delta32(base, uint32(Max32-10)); d != -20 {
		t.Fatalf("near wrap delta = %d, want -20", d)
	}
}

func TestUnwrap33(t *testing.T) {
	base := Ticks(Max33 - 100)
	got := Unwrap33(base, 50)
	if got != Ticks(Max33+50) {
		t.Fatalf("33-bit wrap = %d, want %d", got, Max33+50)
	}
	base = Ticks(1_000_000)
	if Unwrap33(base, 1_000_500) != 1_000_500 {
		t.Fatal("33-bit plain advance failed")
	}
}

func TestDriftEstimator(t *testing.T) {
	// No anchors: identity.
	e := NewDriftEstimator(0)
	s, ic, err := e.Fit()
	if err != nil || s != 1 || ic != 0 {
		t.Fatalf("empty fit = %v %v %v", s, ic, err)
	}
	// One anchor: offset only.
	e.Anchor(100, 105)
	s, ic, _ = e.Fit()
	if s != 1 || ic != 5 {
		t.Fatalf("single anchor = %v %v", s, ic)
	}
	// Perfect linear relation ref = 1.001*raw + 7 on integer ticks.
	e = NewDriftEstimator(MaxDriftPPM)
	for _, raw := range []int64{0, 1000, 2000, 3000} {
		ref := Ticks(raw + raw/1000 + 7)
		e.Anchor(Ticks(raw), ref)
	}
	s, ic, err = e.Fit()
	if err != nil {
		t.Fatalf("fit error: %v", err)
	}
	if d := s - 1.001; d > 1e-9 || d < -1e-9 {
		t.Fatalf("slope = %.9f, want 1.001", s)
	}
	if ic != 7 {
		t.Fatalf("intercept = %d, want 7", ic)
	}
	out := Correct(Ticks(10_000), s, ic)
	if out != Ticks(10017) {
		t.Fatalf("correct = %d, want 10017", out)
	}
}

func TestDriftBoundRejected(t *testing.T) {
	e := NewDriftEstimator(100) // 100 PPM allowed
	e.Anchor(0, 0)
	e.Anchor(1_000_000, 1_010_000) // 1% drift = 10000 PPM
	_, _, err := e.Fit()
	if !errors.Is(err, ErrDriftExceeded) {
		t.Fatalf("want ErrDriftExceeded, got %v", err)
	}
}

func TestDeterministicFit(t *testing.T) {
	anchors := []struct{ raw, ref Ticks }{
		{10, 13}, {50, 51}, {120, 128}, {400, 395}, {900, 912},
	}
	fit := func() (float64, Ticks) {
		e := NewDriftEstimator(0)
		for _, a := range anchors {
			e.Anchor(a.raw, a.ref)
		}
		s, i, _ := e.Fit()
		return s, i
	}
	s1, i1 := fit()
	s2, i2 := fit()
	if s1 != s2 || i1 != i2 {
		t.Fatalf("non-deterministic fit: (%v,%v) vs (%v,%v)", s1, i1, s2, i2)
	}
}
