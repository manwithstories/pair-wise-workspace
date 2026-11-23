package clock

import (
	"errors"
	"math"
	"testing"
)

func TestTrackClockRoundTrip(t *testing.T) {
	tc := TrackClock{TrackID: 1, Timescale: 90000, OriginTicks: 90000, OriginUs: 1_000_000}
	for _, ticks := range []uint64{0, 1, 89999, 90000, 90001, 180000, 90000 * 3600} {
		us, err := tc.ToUnifiedUs(ticks)
		if err != nil {
			t.Fatalf("ToUnifiedUs(%d): %v", ticks, err)
		}
		back, err := tc.FromUnifiedUs(us)
		if err != nil {
			t.Fatalf("FromUnifiedUs(%d): %v", us, err)
		}
		if back != ticks {
			t.Fatalf("round trip ticks %d -> us %d -> ticks %d", ticks, us, back)
		}
	}
}

func TestTrackClockRounding(t *testing.T) {
	// 1 tick at 90kHz is 11.111us -> 11; 5 ticks is 55.5 -> 56.
	tc := TrackClock{TrackID: 2, Timescale: 90000}
	got, err := tc.ToUnifiedUs(1)
	if err != nil || got != 11 {
		t.Fatalf("1 tick: got %d err %v, want 11", got, err)
	}
	got, err = tc.ToUnifiedUs(5)
	if err != nil || got != 56 {
		t.Fatalf("5 ticks: got %d err %v, want 56", got, err)
	}
	// 1000Hz means exact milliseconds.
	tc = TrackClock{TrackID: 3, Timescale: 1000}
	got, err = tc.ToUnifiedUs(1250)
	if err != nil || got != 1_250_000 {
		t.Fatalf("1250 ms ticks: got %d err %v", got, err)
	}
}

func TestTrackClockErrors(t *testing.T) {
	if _, err := (TrackClock{}).ToUnifiedUs(1); !errors.Is(err, ErrNoTimescale) {
		t.Fatalf("zero timescale: %v", err)
	}
	tc := TrackClock{Timescale: math.MaxUint32}
	if _, err := tc.ToUnifiedUs(math.MaxUint64); !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
	tc = TrackClock{Timescale: 90000, OriginTicks: 0}
	if _, err := tc.FromUnifiedUs(math.MaxInt64); !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected overflow from us, got %v", err)
	}
}

func TestMergeAndGaps(t *testing.T) {
	in := []Interval{
		{StartUs: 2000, EndUs: 3000},
		{StartUs: 0, EndUs: 1000},
		{StartUs: 900, EndUs: 1500},
		{StartUs: 5000, EndUs: 5000}, // empty, dropped
		{StartUs: 5000, EndUs: 6000},
	}
	merged := Merge(in)
	if len(merged) != 3 {
		t.Fatalf("merged: %+v", merged)
	}
	if merged[0].EndUs != 1500 || merged[2].StartUs != 5000 {
		t.Fatalf("unexpected merge result: %+v", merged)
	}
	gaps := merged.Gaps(Interval{StartUs: 0, EndUs: 7000}, 1)
	want := []Interval{{StartUs: 1500, EndUs: 2000}, {StartUs: 3000, EndUs: 5000}, {StartUs: 6000, EndUs: 7000}}
	if len(gaps) != len(want) {
		t.Fatalf("gaps: %+v", gaps)
	}
	for i := range want {
		if gaps[i] != want[i] {
			t.Fatalf("gap %d = %+v, want %+v", i, gaps[i], want[i])
		}
	}
	if got := merged.TotalMS(); got != 3.5 {
		t.Fatalf("TotalMS = %v", got)
	}
	if !merged.Contains(1400) || merged.Contains(1600) {
		t.Fatalf("Contains wrong")
	}
}

func TestIntersect(t *testing.T) {
	a := Coverage{{0, 1000}, {2000, 4000}}
	b := Coverage{{500, 3000}}
	got := Intersect(a, b)
	want := Coverage{{500, 1000}, {2000, 3000}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Intersect = %+v", got)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	vals := []float64{40, 10, 30, 20, 50}
	if got := Percentile(vals, 0.5); got != 30 {
		t.Fatalf("p50 = %v", got)
	}
	if got := Percentile(vals, 0.95); got != 50 {
		t.Fatalf("p95 = %v", got)
	}
	if got := Percentile(nil, 0.9); got != 0 {
		t.Fatalf("empty = %v", got)
	}
	// input must not be reordered
	if vals[0] != 40 {
		t.Fatalf("input mutated: %v", vals)
	}
}

func TestErrorModelCounts(t *testing.T) {
	m := NewErrorModel(40, 20)
	m.Record(LocatedError{Kind: ErrMfhdGap, AtUs: 1})
	m.Record(LocatedError{Kind: ErrMfhdGap, AtUs: 2})
	m.Record(LocatedError{Kind: ErrTfdtRegression, AtUs: 3})
	counts := m.Counts()
	if counts["mfhd_gap"] != 2 || counts["tfdt_regression"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}
