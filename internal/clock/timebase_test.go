package clock

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestNewRateRejectsZeroAndNegative(t *testing.T) {
	for _, r := range []Rate{{Num: 0, Den: 1}, {Num: 90000, Den: 0}, {Num: -1, Den: 1}} {
		if _, err := NewRate(r.Num, r.Den); !errors.Is(err, ErrZeroClock) {
			t.Fatalf("rate %+v: want ErrZeroClock, got %v", r, err)
		}
	}
}

func TestNewRateNormalizes(t *testing.T) {
	r, err := NewRate(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if r.Num != 24000 || r.Den != 1 {
		t.Fatalf("normalization = %+v", r)
	}
}

func TestToTimeConversions(t *testing.T) {
	r90k := Rate{Num: 90000, Den: 1}
	// 90,000 ticks at 90kHz = 1 second
	got, err := r90k.ToTime(90000)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nanoseconds() != int64(time.Second) {
		t.Fatalf("90k ticks = %v, want 1s", got.Std())
	}
	// Rational rate 30000/1001: 30 ticks = 1.001 frame interval ≈ 1,001,000,000 ns
	ntsc := Rate{Num: 30000, Den: 1001}
	tm, err := ntsc.ToTime(30)
	if err != nil {
		t.Fatal(err)
	}
	if got := tm.Nanoseconds(); got < 1_000_999_999 || got > 1_001_000_001 {
		t.Fatalf("ntsc conversion = %d ns", got)
	}
}

func TestFromTimeRoundTrip(t *testing.T) {
	r := Rate{Num: 48000, Den: 1}
	for _, ticks := range []int64{0, 1, 47999, 48000, 1234567} {
		tm, err := r.ToTime(ticks)
		if err != nil {
			t.Fatal(err)
		}
		back, err := r.FromTime(tm)
		if err != nil {
			t.Fatal(err)
		}
		if back != ticks {
			t.Fatalf("round trip %d -> %d", ticks, back)
		}
	}
}

func TestToTimeLocatedCarriesContext(t *testing.T) {
	r := Rate{Num: 90000, Den: 1}
	_, e := r.ToTimeLocated(math.MaxInt64, 7, 4096)
	if e == nil || e.TrackID != 7 || e.Offset != 4096 || !errors.Is(e, ErrOverflow) {
		t.Fatalf("located error = %+v", e)
	}
}

func TestRejectsNegativeDuration(t *testing.T) {
	r := Rate{Num: 1, Den: 1}
	if _, err := r.Duration(-5); !errors.Is(err, ErrNegative) {
		t.Fatalf("want ErrNegative, got %v", err)
	}
	g := Range{Start: 100, End: 1}
	if _, err := g.Duration(); !errors.Is(err, ErrNegative) {
		t.Fatalf("inverted range: want ErrNegative, got %v", err)
	}
}

func TestRangeHalfOpenBoundary(t *testing.T) {
	g := Range{Start: Time(0), End: Time(100)}
	if !g.Contains(0) || g.Contains(100) {
		t.Fatal("half-open [start,end) semantics violated")
	}
	if !g.Overlaps(Range{Start: 50, End: 150}) {
		t.Fatal("ranges sharing instants must overlap")
	}
	if g.Overlaps(Range{Start: 100, End: 200}) {
		// Adjacent half-open ranges do not overlap.
		t.Fatal("adjacent [0,100) and [100,200) must not overlap")
	}
}

func TestCmpTicksAcrossClocks(t *testing.T) {
	a := Rate{Num: 90000, Den: 1}
	b := Rate{Num: 48000, Den: 1}
	c, err := CmpTicks(48000, a, 48000, b)
	if err != nil || c != -1 { // 48000@90k = 533ms < 48000@48k = 1s
		t.Fatalf("cmp = %d err=%v", c, err)
	}
	// 90000@90k = 1s; 48000@48k = 1s -> equal
	c, err = CmpTicks(90000, a, 48000, b)
	if err != nil || c != 0 {
		t.Fatalf("equal-instant cmp = %d err=%v", c, err)
	}
}
