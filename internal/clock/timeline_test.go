package clock

import (
	"errors"
	"math"
	"testing"
)

func TestRationalNormalizes(t *testing.T) {
	r := MustRational(60, -120)
	if r.Num != -1 || r.Den != 2 {
		t.Fatalf("got %s", r)
	}
	if _, err := NewRational(1, 0); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("want zero denominator, got %v", err)
	}
}

func TestRescaleNTSCExact(t *testing.T) {
	// 30000/1001 fps onto a 90000 Hz tick axis: 3003 ticks per frame exactly.
	tl := MustTimelineFPS(90000, 1, 30000, 1001)
	got, err := tl.FramesToTicks(30, RoundNearest)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(30 * 3003 / 1); got != want {
		t.Fatalf("FramesToTicks=%d want %d", got, want)
	}
	back, err := tl.TicksToFrames(got, RoundNearest)
	if err != nil || back != 30 {
		t.Fatalf("round trip=%d,%v", back, err)
	}
}

func TestRescaleRoundingModes(t *testing.T) {
	// One tick on a 3 Hz axis is 1/3 s, i.e. 1/3 of a tick on a 1 Hz axis.
	from := MustRational(3, 1)
	to := MustRational(1, 1)
	cases := []struct {
		mode Rounding
		want int64
	}{
		{RoundNearest, 0}, // 0.333 -> 0
		{RoundDown, 0},
		{RoundUp, 1},
		{RoundTowardZero, 0},
	}
	for _, c := range cases {
		got, err := Rescale(1, from, to, c.mode)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: got %d want %d", c.mode, got, c.want)
		}
	}
	// 2/3 -> nearest 1, and negatives mirror the same policies.
	if got, _ := Rescale(2, from, to, RoundNearest); got != 1 {
		t.Errorf("2/3 nearest=%d", got)
	}
	if got, _ := Rescale(-2, from, to, RoundNearest); got != -1 {
		t.Errorf("-2/3 nearest=%d", got)
	}
	if got, _ := Rescale(-2, from, to, RoundDown); got != -1 {
		t.Errorf("-2/3 down=%d", got)
	}
	if got, _ := Rescale(-2, from, to, RoundUp); got != 0 {
		t.Errorf("-2/3 up=%d", got)
	}
}

func TestRescaleOverflow(t *testing.T) {
	// Scaling MaxInt64 up by 1000 ticks/s cannot fit in int64.
	_, err := Rescale(math.MaxInt64, MustRational(1, 1), MustRational(1000, 1), RoundNearest)
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("want overflow, got %v", err)
	}
	// The identity conversion is exact and must not overflow.
	if got, err := Rescale(math.MaxInt64, MustRational(1, 1), MustRational(1, 1), RoundNearest); err != nil || got != math.MaxInt64 {
		t.Fatalf("identity conversion=%d,%v", got, err)
	}
}

func TestZeroAndNegativeRatesRejected(t *testing.T) {
	if _, err := NewTimebase(MustRational(0, 1)); !errors.Is(err, ErrInvalidRate) {
		t.Fatal("zero rate accepted")
	}
	if _, err := NewTimeline(MustRational(90000, 1), MustRational(0, 1)); !errors.Is(err, ErrInvalidRate) {
		t.Fatal("zero fps accepted")
	}
}

func TestInterval(t *testing.T) {
	iv, err := NewInterval(100, 50)
	if err != nil || iv.Duration() != 50 {
		t.Fatalf("iv=%v err=%v", iv, err)
	}
	if _, err := NewInterval(1, -1); !errors.Is(err, ErrNegativeDuration) {
		t.Fatal("negative duration accepted")
	}
	if _, err := NewInterval(math.MaxInt64, 1); !errors.Is(err, ErrOverflow) {
		t.Fatal("overflow accepted")
	}
	a := Interval{Start: 0, End: 10}
	b := Interval{Start: 5, End: 20}
	if !a.Overlaps(b) {
		t.Fatal("expected overlap")
	}
	if x, ok := a.Intersect(b); !ok || x.Start != 5 || x.End != 10 {
		t.Fatalf("intersect=%v,%v", x, ok)
	}
	if a.Overlaps(Interval{Start: 10, End: 12}) {
		t.Fatal("half-open intervals must not overlap at the boundary")
	}
}

func TestMilliseconds(t *testing.T) {
	tl := MustTimelineFPS(90000, 1, 25, 1)
	if got := tl.TicksToMillis(9000); got != 100 {
		t.Fatalf("9000 ticks at 90k = %v ms", got)
	}
	if got, want := tl.TicksToMillis(3003), 33.36666666666667; math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v want %v", got, want)
	}
	if ticks, err := tl.MillisToTicks(100, RoundNearest); err != nil || ticks != 9000 {
		t.Fatalf("MillisToTicks=%d,%v", ticks, err)
	}
}

// MustTimelineFPS is a test helper mirroring the production constructor.
func MustTimelineFPS(tickNum, tickDen, fpsNum, fpsDen int64) Timeline {
	tl, err := NewTimeline(MustRational(tickNum, tickDen), MustRational(fpsNum, fpsDen))
	if err != nil {
		panic(err)
	}
	return tl
}
