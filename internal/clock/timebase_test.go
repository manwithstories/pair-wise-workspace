package clock

import (
	"errors"
	"math"
	"testing"
)

func TestNewRational(t *testing.T) {
	r, err := NewRational(2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if r != (Rational{Num: 1, Den: 2}) {
		t.Fatalf("normalization: %+v", r)
	}
	for _, c := range [][2]int64{{0, 1}, {1, 0}, {-1, 10}, {10, -1}} {
		if _, err := NewRational(c[0], c[1]); !errors.Is(err, ErrInvalidTimeBase) {
			t.Fatalf("(%d,%d) accepted: %v", c[0], c[1], err)
		}
	}
}

func TestConvertExactAndRounded(t *testing.T) {
	ms := Rational{Num: 1, Den: 1000}
	us := Microsecond
	got, err := Convert(1, ms, us)
	if err != nil || got != 1000 {
		t.Fatalf("1/1000->1/1e6: %d %v", got, err)
	}
	got, err = Convert(1, us, ms)
	if err != nil || got != 0 {
		t.Fatalf("round down half: %d %v", got, err)
	}
	got, err = Convert(1500, us, ms)
	if err != nil || got != 2 {
		t.Fatalf("round half away: %d %v", got, err)
	}
	got, err = Convert(-1500, us, ms)
	if err != nil || got != -2 {
		t.Fatalf("round half negative: %d %v", got, err)
	}
	// 1/90000 -> 1/48000 = 8/15, exact big.Int math.
	got, err = Convert(90000, Rational{1, 90000}, Rational{1, 48000})
	if err != nil || got != 48000 {
		t.Fatalf("90000->48000: %d %v", got, err)
	}
	if _, err := Convert(0, Rational{}, us); !errors.Is(err, ErrInvalidTimeBase) {
		t.Fatalf("invalid base accepted: %v", err)
	}
	if got, err := Convert(NoTimestamp, ms, us); err != nil || got != NoTimestamp {
		t.Fatalf("no timestamp: %d %v", got, err)
	}
}

func TestConvertOverflow(t *testing.T) {
	_, err := Convert(math.MaxInt64, Rational{Num: 1000, Den: 1}, Rational{Den: 1, Num: 1})
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
	e, ok := AsError(err)
	if !ok || e.Code != CodeOverflow {
		t.Fatalf("unified error missing: %v", err)
	}
}

func TestAddMulTicks(t *testing.T) {
	if _, err := AddTicks(math.MaxInt64, 1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("add overflow: %v", err)
	}
	if _, err := MulTicks(math.MaxInt64, 2); !errors.Is(err, ErrOverflow) {
		t.Fatalf("mul overflow: %v", err)
	}
	if _, err := MulTicks(10, -1); !errors.Is(err, ErrNegativeDuration) {
		t.Fatalf("negative factor: %v", err)
	}
	if v, err := AddTicks(NoTimestamp, 5); err != nil || v != NoTimestamp {
		t.Fatalf("nopts add: %d %v", v, err)
	}
}

func TestTimelineValidation(t *testing.T) {
	_, err := NewTimeline([]TrackSpec{{Track: 0, TimeBase: Rational{1, 1000}, Duration: -5}})
	if !errors.Is(err, ErrNegativeDuration) {
		t.Fatalf("negative duration: %v", err)
	}
	e, _ := AsError(err)
	if e.Track != 0 || e.Position != -5 || e.TimeBase != (Rational{1, 1000}) {
		t.Fatalf("location lost: %+v", e)
	}
	_, err = NewTimeline([]TrackSpec{{Track: 0, TimeBase: Rational{}}})
	if !errors.Is(err, ErrInvalidTimeBase) {
		t.Fatalf("invalid tb: %v", err)
	}
	_, err = NewTimeline([]TrackSpec{
		{Track: 0, TimeBase: Rational{1, 1000}},
		{Track: 0, TimeBase: Rational{1, 1000}},
	})
	if err == nil {
		t.Fatal("duplicate track accepted")
	}
}

func TestTimelineUnification(t *testing.T) {
	tl, err := NewTimeline([]TrackSpec{
		{Track: 0, TimeBase: Rational{1, 1000}, Start: 0, Duration: 1000},
		{Track: 1, TimeBase: Rational{1, 48000}, Start: 0, Duration: 48000},
		{Track: 2, TimeBase: Rational{1, 90000}, Start: 9000, Duration: 90000},
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := tl.ToUnified(2, 9000)
	if err != nil || u != 0 {
		t.Fatalf("track start should map to 0: %d %v", u, err)
	}
	u, err = tl.ToUnified(2, 99000)
	if err != nil || u != 1_000_000 {
		t.Fatalf("track 1s: %d %v", u, err)
	}
	back, err := tl.ToTrack(2, 1_000_000)
	if err != nil || back != 99000 {
		t.Fatalf("roundtrip: %d %v", back, err)
	}
	if _, err := tl.ToUnified(2, -1); !errors.Is(err, ErrInvalidTimestamp) {
		t.Fatalf("negative pts: %v", err)
	}
	if _, err := tl.ToUnified(99, 0); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("unknown track: %v", err)
	}
	total, err := tl.TotalDuration()
	if err != nil || total != 1_000_000 {
		t.Fatalf("total: %d %v", total, err)
	}
}

func TestTimelineRejectsOverflowWithLocation(t *testing.T) {
	tl, err := NewTimeline([]TrackSpec{{Track: 3, TimeBase: Rational{1, 1000}, Start: 0}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tl.ToUnified(3, math.MaxInt64)
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected overflow: %v", err)
	}
	e, _ := AsError(err)
	if e.Track != 3 {
		t.Fatalf("track lost: %+v", e)
	}
}

func TestMergeIntervals(t *testing.T) {
	n, err := MergeIntervals([][2]int64{{0, 1000}, {1000, 2500}})
	if err != nil || n != 2500 {
		t.Fatalf("merge: %d %v", n, err)
	}
	if _, err := MergeIntervals([][2]int64{{0, 1000}, {500, 2000}}); !errors.Is(err, ErrTimelineConflict) {
		t.Fatalf("overlap accepted: %v", err)
	}
	if _, err := MergeIntervals([][2]int64{{1000, 500}}); !errors.Is(err, ErrNegativeDuration) {
		t.Fatalf("inverted accepted: %v", err)
	}
}

func TestErrorFormat(t *testing.T) {
	err := (&Error{Code: CodeInvalidTimestamp, Op: "decode", Track: 4, Position: 9000, TimeBase: Rational{1, 90000}}).
		WithPosition(4, 9000, Rational{1, 90000})
	s := err.Error()
	for _, want := range []string{"decode", "invalid_timestamp", "track=4", "pos=9000", "0.100000s"} {
		if !contains(s, want) {
			t.Fatalf("%q missing from %q", want, s)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
