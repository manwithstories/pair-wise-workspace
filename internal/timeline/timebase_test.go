package timeline

import (
	"errors"
	"math"
	"testing"

	"fastpost/internal/model"
)

func TestToCommonWholeSecond(t *testing.T) {
	tb, err := New("cam-a", Rational{Num: 48000, Den: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tb.ToCommon(48000) // one second of audio samples
	if err != nil {
		t.Fatal(err)
	}
	if got != CommonRate {
		t.Fatalf("one second = %d ticks, want %d", got, CommonRate)
	}
}

func TestToCommonFractionalFrame(t *testing.T) {
	// 29.97 fps video clock: one frame is 90000*1001/30000 = 3003 ticks.
	tb, err := New("cam-b", Rational{Num: 30000, Den: 1001}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tb.ToCommon(1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3003 {
		t.Fatalf("one frame = %d ticks, want 3003", got)
	}
}

func TestToCommonAtAppliesOrigin(t *testing.T) {
	tb, err := New("cam-c", Rational{Num: 90000, Den: 1}, 12_345)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tb.ToCommonAt(7)
	if err != nil {
		t.Fatal(err)
	}
	if got != 12_352 {
		t.Fatalf("origin-relative timestamp = %d, want 12352", got)
	}
}

func TestNegativeDuration(t *testing.T) {
	tb, _ := New("cam-d", Rational{Num: 48000, Den: 1}, 0)
	if _, err := tb.ToCommon(-1); !isFault(err, model.FaultNegativeDuration) {
		t.Fatalf("err = %v, want %s", err, model.FaultNegativeDuration)
	}
	if _, err := tb.ToCommonAt(-9); !isFault(err, model.FaultUnmappableEvent) {
		t.Fatalf("err = %v, want unmappable event", err)
	}
}

func TestOverflow(t *testing.T) {
	tb, _ := New("cam-e", Rational{Num: 1, Den: 1}, 0)
	if _, err := tb.ToCommon(math.MaxInt64); !isFault(err, model.FaultOverflow) {
		t.Fatalf("err = %v, want overflow", err)
	}
	tb2, _ := New("cam-f", Rational{Num: 1, Den: 1}, math.MaxInt64-3)
	if _, err := tb2.ToCommonAt(10); !isFault(err, model.FaultOverflow) {
		t.Fatalf("err = %v, want origin overflow", err)
	}
}

func TestFromCommonRoundTrip(t *testing.T) {
	// 44.1 kHz audio must round cleanly over one second.
	tb, _ := New("cam-g", Rational{Num: 44100, Den: 1}, 0)
	units, err := tb.FromCommon(CommonRate)
	if err != nil {
		t.Fatal(err)
	}
	if units != 44100 {
		t.Fatalf("one second in 44.1k = %d units, want 44100", units)
	}
	back, err := tb.ToCommon(44100)
	if err != nil || back != CommonRate {
		t.Fatalf("round trip = %d, %v; want %d", back, err, CommonRate)
	}
}

func TestInvalidTimebase(t *testing.T) {
	if _, err := New("cam-h", Rational{Num: 0, Den: 1}, 0); !isFault(err, model.FaultUnmappableEvent) {
		t.Fatalf("err = %v, want unmappable event", err)
	}
	if _, err := New("cam-i", Rational{Num: 1, Den: 0}, 0); !isFault(err, model.FaultUnmappableEvent) {
		t.Fatalf("err = %v, want unmappable event", err)
	}
}

func TestCompareRational(t *testing.T) {
	cases := []struct {
		a, b Rational
		want int
	}{
		{Rational{30000, 1001}, Rational{30, 1}, -1},
		{Rational{48000, 1}, Rational{44100, 1}, 1},
		{Rational{1, 2}, Rational{2, 4}, 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Fatalf("Compare(%v,%v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSkewCollapsesToZero(t *testing.T) {
	// Two cameras with different origins; after mapping to the common
	// timeline the same instant must land on the same tick.
	ref, _ := New("ref", Rational{Num: 90000, Den: 1}, 0)
	slave, _ := New("slave", Rational{Num: 90000, Den: 1}, 500)
	a, _ := ref.ToCommonAt(1000)
	b, _ := slave.ToCommonAt(500)
	if a != b {
		t.Fatalf("skew not absorbed: %d vs %d", a, b)
	}
}

func isFault(err error, kind model.FaultKind) bool {
	var f model.Fault
	return errors.As(err, &f) && f.Kind == kind
}
