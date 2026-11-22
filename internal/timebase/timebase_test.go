package timebase

import (
	"errors"
	"math"
	"testing"

	"mediarun/internal/mediaerr"
)

func TestNewNormalizesAndReduces(t *testing.T) {
	r := New(2, -4)
	if r.Num != -1 || r.Den != 2 {
		t.Fatalf("got %v, want -1/2 (sign flip + reduction)", r)
	}
	if got := New(12, 18); got.Num != 2 || got.Den != 3 {
		t.Fatalf("got %v, want 2/3", got)
	}
}

func TestNewZeroDenominatorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on zero denominator")
		}
	}()
	New(1, 0)
}

func TestArithmeticExact(t *testing.T) {
	third := New(1, 3)
	sum := third.Add(third).Add(third)
	if !sum.Equal(FromInt(1)) {
		t.Fatalf("1/3*3 = %v, want exactly 1 (no float drift)", sum)
	}
	if got := New(7, 6).Sub(New(1, 6)); !got.Equal(FromInt(1)) {
		t.Fatalf("sub got %v", got)
	}
	if got := New(2, 3).Mul(New(3, 4)); !got.Equal(New(1, 2)) {
		t.Fatalf("mul got %v, want 1/2", got)
	}
	if got := New(1, 2).Div(New(3, 4)); !got.Equal(New(2, 3)) {
		t.Fatalf("div got %v, want 2/3", got)
	}
}

func TestCmpAndSign(t *testing.T) {
	cases := []struct {
		a, b Rational
		want int
	}{
		{New(1, 3), New(1, 2), -1},
		{New(5, 10), New(1, 2), 0},
		{New(7, 2), New(3, 1), 1},
		{Zero, New(-1, 7), 1},
	}
	for _, c := range cases {
		if got := c.a.Cmp(c.b); got != c.want {
			t.Errorf("%v.Cmp(%v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if !New(-3, 4).IsNegative() || Zero.IsNegative() {
		t.Error("IsNegative wrong")
	}
}

func TestNegativeDurationStructuredError(t *testing.T) {
	err := RequireNonNegative(New(-5, 2), "video/0")
	if err == nil {
		t.Fatal("want error for negative duration")
	}
	if !mediaerr.Is(err, mediaerr.CodeNegativeDuration) {
		t.Fatalf("want NEGATIVE_DURATION, got %v", err)
	}
	var se *mediaerr.Error
	if !errors.As(err, &se) || se.Track != "video/0" {
		t.Fatalf("structured track context missing: %v", err)
	}
	if err := RequireNonNegative(Zero, "a"); err != nil {
		t.Fatalf("zero must be allowed, got %v", err)
	}
}

func TestTimebaseConversionsRoundTrip(t *testing.T) {
	// 90 kHz transport clock: 90000 ticks = 1 s exactly.
	tb := NewTimebase(90000)
	oneSec := tb.ToSeconds(90000)
	if !oneSec.Equal(FromInt(1)) {
		t.Fatalf("90000 ticks = %v, want 1s", oneSec)
	}
	// MP4-style timescale=30000: one 29.97 fps frame spans 1001 ticks,
	// i.e. 1001/30000 s — slightly LONGER than a nominal 1/30 s frame.
	tb30k := NewTimebase(30000)
	frame := tb30k.ToSeconds(1001)
	if frame.Cmp(New(1, 30)) <= 0 {
		t.Fatalf("frame window %v must be > 1/30 s for 29.97 fps", frame)
	}
	ticks, residual := tb30k.FromSeconds(frame)
	if ticks != 1001 || !residual.IsZero() {
		t.Fatalf("round trip got ticks=%d residual=%v", ticks, residual)
	}
}

func TestFromSecondsRoundingAndResidual(t *testing.T) {
	tb := NewTimebase(10) // 100 ms ticks
	// 0.25 s -> 2.5 ticks, rounds to 3, residual -0.5 tick.
	ticks, residual := tb.FromSeconds(New(1, 4))
	if ticks != 3 {
		t.Fatalf("ticks=%d, want 3 (half-up)", ticks)
	}
	if !residual.Equal(New(-1, 2)) {
		t.Fatalf("residual=%v, want -1/2", residual)
	}
}

func TestMillisReporting(t *testing.T) {
	if got := Millis(New(1234, 1000)); got != 1234 {
		t.Fatalf("millis=%d, want 1234", got)
	}
	// 1/3 s rounds to 333 ms, never a NaN.
	if got := Millis(New(1, 3)); got != 333 {
		t.Fatalf("millis=%d, want 333", got)
	}
	if math.IsNaN(Zero.Float64()) {
		t.Fatal("zero float must be defined")
	}
}
