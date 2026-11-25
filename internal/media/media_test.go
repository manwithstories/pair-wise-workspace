package media

import (
	"errors"
	"math"
	"testing"
)

func TestRatReducesAndNormalizesSign(t *testing.T) {
	cases := []struct {
		n, d  int64
		wantN int64
		wantD int64
	}{
		{2, 4, 1, 2},
		{-6, -9, 2, 3},
		{6, -9, -2, 3},
		{0, 7, 0, 1},
	}
	for _, c := range cases {
		r, f := Rat(c.n, c.d)
		if f != nil {
			t.Fatalf("Rat(%d,%d) unexpected fault %v", c.n, c.d, f)
		}
		if r.Num != c.wantN || r.Den != c.wantD {
			t.Fatalf("Rat(%d,%d)=%d/%d want %d/%d", c.n, c.d, r.Num, r.Den, c.wantN, c.wantD)
		}
	}
}

func TestRatRejectsZeroDenominator(t *testing.T) {
	_, f := Rat(1, 0)
	if f == nil || f.Code != CodeInvalidTimebase {
		t.Fatalf("want invalid_timebase, got %v", f)
	}
}

func TestRationalExactCompareNoFloatDrift(t *testing.T) {
	third := MustRat(1, 3)
	twoThirds, f := third.Add(third)
	if f != nil {
		t.Fatal(f)
	}
	threeThirds, f := twoThirds.Add(third)
	if f != nil {
		t.Fatal(f)
	}
	if threeThirds.Cmp(MustRat(1, 1)) != 0 {
		t.Fatalf("1/3*3 via Add = %s, want exactly 1", threeThirds)
	}
	// 0.1 repeating style accumulation must stay exact.
	tenth := MustRat(1, 10)
	sum := MustRat(0, 1)
	for i := 0; i < 10; i++ {
		sum, f = sum.Add(tenth)
		if f != nil {
			t.Fatal(f)
		}
	}
	if sum.Cmp(MustRat(1, 1)) != 0 {
		t.Fatalf("ten tenths = %s, want 1 exactly", sum)
	}
	if MustRat(1, 3).Cmp(MustRat(333333, 1000000)) <= 0 {
		t.Fatal("1/3 must compare greater than 0.333333 exactly")
	}
}

func TestRationalAddOverflowRejected(t *testing.T) {
	big := Rational{Num: math.MaxInt64, Den: 1}
	if _, f := big.Add(Rational{Num: 1, Den: 1}); f == nil || f.Code != CodeOverflow {
		t.Fatalf("want overflow, got %v", f)
	}
	// Sign normalization that cannot fit must be rejected.
	if _, f := Rat(1, math.MinInt64); f == nil || f.Code != CodeOverflow {
		t.Fatalf("want overflow for MinInt64 denominator sign flip, got %v", f)
	}
}

func TestClockRejectsZeroAndNegativeRate(t *testing.T) {
	if _, f := NewClock(0); f == nil || f.Code != CodeInvalidTimebase {
		t.Fatalf("zero rate: %v", f)
	}
	if _, f := NewClock(-48000); f == nil || f.Code != CodeInvalidTimebase {
		t.Fatalf("negative rate: %v", f)
	}
}

func TestClockPTSAndRange(t *testing.T) {
	c, f := NewClock(48000)
	if f != nil {
		t.Fatal(f)
	}
	pts, f := c.PTS(48000)
	if f != nil || pts.Cmp(MustRat(1, 1)) != 0 {
		t.Fatalf("48000 ticks = %s, %v; want 1", pts, f)
	}
	rng, f := c.Range(0, 960) // 20 ms
	if f != nil {
		t.Fatal(f)
	}
	dur, f := rng.Duration()
	if f != nil || dur.Cmp(MustRat(1, 50)) != 0 {
		t.Fatalf("duration = %s, %v want 1/50", dur, f)
	}
	if _, f := c.Range(10, 9); f == nil || f.Code != CodeBackwardRange {
		t.Fatalf("backwards range must be rejected, got %v", f)
	}
}

func TestRescaleTicks(t *testing.T) {
	got, f := RescaleTicks(48000, 48000, 96000, true)
	if f != nil || got != 96000 {
		t.Fatalf("rescale = %d %v", got, f)
	}
	if _, f := RescaleTicks(1, 48000, 44100, true); f == nil {
		t.Fatal("non-exact rescale should be rejected in exact mode")
	}
	floored, f := RescaleTicks(1, 48000, 44100, false)
	if f != nil || floored != 0 {
		t.Fatalf("floor rescale = %d %v want 0", floored, f)
	}
	if _, f := RescaleTicks(10, 0, 48000, false); f == nil || f.Code != CodeInvalidTimebase {
		t.Fatalf("zero fromRate: %v", f)
	}
	if _, f := RescaleTicks(math.MaxInt64, 1, 2, false); f == nil || f.Code != CodeOverflow {
		t.Fatalf("overflow rescale: %v", f)
	}
}

func TestFaultExitCodesAndExtraction(t *testing.T) {
	cases := []struct {
		code Code
		want int
	}{
		{CodeParseError, 2},
		{CodeEmptyChannel, 2},
		{CodeClipping, 2},
		{CodeIncompleteRange, 2},
		{CodeIntegrity, 3},
		{CodeChecksumMismatch, 3},
		{CodeInterrupted, 75},
	}
	for _, c := range cases {
		err := NewFault(c.code, "x")
		if got := ExitCode(err); got != c.want {
			t.Fatalf("code %s exit=%d want %d", c.code, got, c.want)
		}
	}
	if ExitCode(nil) != 0 {
		t.Fatal("nil must map to 0")
	}
	wrapped := NewFault(CodeParseError, "bad box").WithTrack(2)
	if _, ok := AsFault(wrapped); !ok {
		t.Fatal("AsFault failed")
	}
	if !errors.Is(wrapped, NewFault(CodeParseError, "")) {
		t.Fatal("errors.Is should compare by code")
	}
	if ExitCode(errors.New("plain")) != 1 {
		t.Fatal("plain error must map to 1")
	}
}

func TestFaultCarriesTrackAndRange(t *testing.T) {
	rng, _ := NewClock(48000)
	tr, f := rng.Range(0, 4800)
	if f != nil {
		t.Fatal(f)
	}
	flt := NewFault(CodeIncompleteRange, "missing frames").WithTrack(3).WithRange(tr.Start, tr.End)
	if flt.TrackID != 3 || flt.Range == nil {
		t.Fatalf("fault missing track/range: %+v", flt)
	}
	if flt.Retryable {
		t.Fatal("incomplete_range should default to non-retryable")
	}
	if flt.Error() == "" {
		t.Fatal("Error() empty")
	}
}
