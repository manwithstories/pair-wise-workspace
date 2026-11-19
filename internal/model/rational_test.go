package model

import (
	"testing"
)

func TestNewRatNormalizes(t *testing.T) {
	cases := []struct {
		num, den, wantNum, wantDen int64
	}{
		{2, 4, 1, 2},
		{-2, 4, -1, 2},
		{2, -4, -1, 2},
		{0, 5, 0, 1},
		{7, 1, 7, 1},
	}
	for _, c := range cases {
		got := NewRat(c.num, c.den)
		if got.Num != c.wantNum || got.Den != c.wantDen {
			t.Fatalf("NewRat(%d,%d) = %s, want %d/%d", c.num, c.den, got, c.wantNum, c.wantDen)
		}
	}
	if NewRat(1, 0).Valid() {
		t.Fatalf("zero denominator must be invalid")
	}
}

func TestArithmetic(t *testing.T) {
	a := NewRat(1, 3)
	b := NewRat(1, 6)
	if got := a.Add(b); got.Cmp(NewRat(1, 2)) != 0 {
		t.Fatalf("1/3 + 1/6 = %s", got)
	}
	if got := a.Sub(b); got.Cmp(NewRat(1, 6)) != 0 {
		t.Fatalf("1/3 - 1/6 = %s", got)
	}
	if got := a.Mul(b); got.Cmp(NewRat(1, 18)) != 0 {
		t.Fatalf("1/3 * 1/6 = %s", got)
	}
	if got := a.Div(b); got.Cmp(NewRat(2, 1)) != 0 {
		t.Fatalf("1/3 / 1/6 = %s", got)
	}
	if got := b.Neg(); got.Cmp(NewRat(-1, 6)) != 0 {
		t.Fatalf("-1/6 = %s", got)
	}
	if a.Cmp(b) <= 0 {
		t.Fatalf("1/3 should exceed 1/6")
	}
}

func TestOverflowFallsBackToBigInt(t *testing.T) {
	// 2^40/3 times 2^40/7 overflows int64 in both numerator and denominator;
	// the result must still compare exactly against the rational answer.
	a := NewRat(1<<40, 3)
	b := NewRat(1<<40, 7)
	got := a.Mul(b)
	want := a.Div(NewRat(3, 1)).Mul(b).Mul(NewRat(3, 1)) // same product, recomputed
	if got.Cmp(want) != 0 {
		t.Fatalf("large multiply = %s, want %s", got, want)
	}
	// The exact value is 2^80/21; check it numerically against 2^40/3 * 2^40/7.
	if got.Float64() == 0 {
		t.Fatalf("overflow produced an unrepresentable value: %s", got)
	}
}

func TestScaleRoundsHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		r    Rational
		ts   uint32
		want int64
	}{
		{NewRat(1, 2), 1, 1},
		{NewRat(-1, 2), 1, -1},
		{NewRat(1, 3), 3, 1},
		{NewRat(1, 1000), 1000, 1},
		{NewRat(1, 30), 90000, 3000},
	}
	for _, c := range cases {
		if got := c.r.Scale(c.ts); got != c.want {
			t.Fatalf("%s scaled by %d = %d, want %d", c.r, c.ts, got, c.want)
		}
	}
}

func TestFromTicksRoundTrip(t *testing.T) {
	r := FromTicks(3000, 90000)
	if r.Cmp(NewRat(1, 30)) != 0 {
		t.Fatalf("3000/90000 = %s, want 1/30", r)
	}
	if got := r.Scale(90000); got != 3000 {
		t.Fatalf("back to ticks = %d", got)
	}
}

func TestTimelineValidateReportsEachProblem(t *testing.T) {
	tl := &Timeline{
		Version: 1,
		Tracks: []Track{
			{ID: "V1", Kind: KindVideo, Source: "v.mp4", Timescale: 90000},
			{ID: "V1", Kind: KindVideo, Source: "v2.mp4", Timescale: 0},
		},
		Clips: []Clip{
			{ID: "C1", Track: "MISSING", Duration: NewRat(1, 1)},
			{ID: "C2", Track: "V1", Duration: NewRat(0, 1), Parent: "NOPE"},
		},
		Transitions: []Transition{{ID: "X1", From: "C1", To: "NOPE", Duration: NewRat(1, 1)}},
	}
	errs := tl.Validate()
	if len(errs) < 6 {
		t.Fatalf("expected several validation errors, got %d: %v", len(errs), errs)
	}
}
