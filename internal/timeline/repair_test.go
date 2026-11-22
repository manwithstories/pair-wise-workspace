package timeline

import (
	"fmt"
	"sort"
	"testing"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
)

// ms builds an exact millisecond duration from a small decimal literal.
func ms(v float64) clock.Rational {
	return clock.MustParseDecimal(fmt.Sprintf("%g", v/1000.0))
}

func frames(track uint64, times ...float64) []Frame {
	out := make([]Frame, len(times))
	for i, t := range times {
		out[i] = Frame{Track: track, Original: ms(t), Offset: int64(i * 10)}
	}
	return out
}

func TestCleanTimelineHasNoRepairs(t *testing.T) {
	tl := New(ms(1), Policy{})
	tl.AddTrack(&TrackInfo{Number: 1, Kind: "video", Frames: frames(1, 0, 40, 80, 120)})
	res := tl.Repair()
	if len(res.Repairs) != 0 || res.Regress != 0 || res.Duplicates != 0 {
		t.Fatalf("unexpected repairs: %+v", res.Repairs)
	}
	if res.Faults.Len() != 0 {
		t.Fatalf("faults: %v", res.Faults.Faults())
	}
}

func TestRegressionClampedAndRecorded(t *testing.T) {
	tl := New(ms(1), Policy{})
	// 0, 40, 20 (regress), 80
	tl.AddTrack(&TrackInfo{Number: 1, Kind: "video", Frames: frames(1, 0, 40, 20, 80)})
	res := tl.Repair()
	if res.Regress != 1 {
		t.Fatalf("regress=%d", res.Regress)
	}
	if len(res.Repairs) != 1 {
		t.Fatalf("repairs=%+v", res.Repairs)
	}
	r := res.Repairs[0]
	if r.Kind != "timestamp-regression" || r.BeforeUsec != 20000 || r.AfterUsec != 41000 || r.DeltaUsec != 21000 {
		t.Fatalf("repair=%+v", r)
	}
	if res.Faults.Count(fault.KindTimestampRegress) != 1 {
		t.Fatalf("faults: %v", res.Faults.Faults())
	}
	// Output must be strictly increasing.
	prev := clock.Rational{}
	for i, f := range res.Tracks[1].Frames {
		if i > 0 && f.Repaired.Cmp(prev) <= 0 {
			t.Fatalf("frame %d not increasing: %s", i, f.Repaired)
		}
		prev = f.Repaired
	}
}

func TestDuplicateNudged(t *testing.T) {
	tl := New(ms(1), Policy{})
	tl.AddTrack(&TrackInfo{Number: 2, Kind: "audio", Frames: frames(2, 0, 20, 20, 40)})
	res := tl.Repair()
	if res.Duplicates != 1 || len(res.Repairs) != 1 {
		t.Fatalf("dup=%d repairs=%+v", res.Duplicates, res.Repairs)
	}
	if res.Repairs[0].Kind != "timestamp-duplicate" || res.Repairs[0].AfterUsec != 21000 {
		t.Fatalf("repair=%+v", res.Repairs[0])
	}
}

func TestDuplicateDropRule(t *testing.T) {
	tl := New(ms(1), Policy{DuplicateRule: "drop"})
	tl.AddTrack(&TrackInfo{Number: 2, Kind: "audio", Frames: frames(2, 0, 20, 20, 40)})
	res := tl.Repair()
	if !res.Tracks[2].Frames[2].Missing {
		t.Fatal("duplicate should be marked missing under drop rule")
	}
	if res.Tracks[2].Visible() != 3 {
		t.Fatalf("visible=%d", res.Tracks[2].Visible())
	}
}

func TestRegressDropRule(t *testing.T) {
	tl := New(ms(1), Policy{RegressRule: "drop"})
	tl.AddTrack(&TrackInfo{Number: 1, Kind: "video", Frames: frames(1, 0, 40, 20, 80)})
	res := tl.Repair()
	if !res.Tracks[1].Frames[2].Missing {
		t.Fatal("regressed frame should be marked missing")
	}
}

func TestRepairDeterministic(t *testing.T) {
	build := func() []Frame {
		f := frames(1, 0, 40, 20, 20, 10, 80, 79)
		// shuffle deterministically by reversing
		for i, j := 0, len(f)-1; i < j; i, j = i+1, j-1 {
			f[i], f[j] = f[j], f[i]
		}
		return f
	}
	run := func() *Result {
		tl := New(ms(1), Policy{})
		tl.AddTrack(&TrackInfo{Number: 1, Kind: "video", Frames: build()})
		return tl.Repair()
	}
	a, b := run(), run()
	if len(a.Repairs) != len(b.Repairs) {
		t.Fatalf("repair counts differ: %d vs %d", len(a.Repairs), len(b.Repairs))
	}
	for i := range a.Repairs {
		if a.Repairs[i] != b.Repairs[i] {
			t.Fatalf("repair %d differs: %+v vs %+v", i, a.Repairs[i], b.Repairs[i])
		}
	}
	// Output timestamps must be monotonically non-decreasing.
	for _, tr := range a.Tracks {
		for i := 1; i < len(tr.Frames); i++ {
			if tr.Frames[i].Repaired.Cmp(tr.Frames[i-1].Repaired) < 0 {
				t.Fatalf("non-monotonic output at %d", i)
			}
		}
	}
}

func TestCrossTrackDriftDetected(t *testing.T) {
	tl := New(ms(1), Policy{MaxDrift: ms(2)})
	tl.AddTrack(&TrackInfo{Number: 1, Kind: "video", Frames: frames(1, 0, 40)})
	tl.AddTrack(&TrackInfo{Number: 2, Kind: "audio", Frames: frames(2, 5, 45)})
	res := tl.Repair()
	if len(res.Drifts) != 1 {
		t.Fatalf("drifts=%+v", res.Drifts)
	}
	d := res.Drifts[0]
	if d.SkewUsec != -5000 && d.SkewUsec != 5000 {
		t.Fatalf("skew=%d", d.SkewUsec)
	}
	if !d.Beyond {
		t.Fatal("5ms skew should exceed 2ms limit")
	}
	if res.Faults.Count(fault.KindDrift) != 1 {
		t.Fatalf("faults: %v", res.Faults.Faults())
	}
}

func TestDriftWithinLimitSilent(t *testing.T) {
	tl := New(ms(1), Policy{MaxDrift: ms(10)})
	tl.AddTrack(&TrackInfo{Number: 1, Kind: "video", Frames: frames(1, 0, 40)})
	tl.AddTrack(&TrackInfo{Number: 2, Kind: "audio", Frames: frames(2, 5, 45)})
	res := tl.Repair()
	if len(res.Drifts) != 0 || res.Faults.Count(fault.KindDrift) != 0 {
		t.Fatalf("drifts=%+v faults=%v", res.Drifts, res.Faults.Faults())
	}
}

func TestMissingFrameKeepsSlot(t *testing.T) {
	tl := New(ms(1), Policy{})
	f := frames(1, 0, 40, 80)
	f[1].Missing = true
	tl.AddTrack(&TrackInfo{Number: 1, Kind: "video", Frames: f})
	res := tl.Repair()
	if res.Tracks[1].Visible() != 2 {
		t.Fatalf("visible=%d", res.Tracks[1].Visible())
	}
	start, end := res.Tracks[1].StartEnd()
	if start.Millis() != 0 || end.Millis() != 80 {
		t.Fatalf("extent %v..%v", start.Millis(), end.Millis())
	}
	// Missing frame must not trigger a repair.
	if len(res.Repairs) != 0 {
		t.Fatalf("repairs=%+v", res.Repairs)
	}
}

func TestTrackOrderIsSortedByNumber(t *testing.T) {
	tl := New(ms(1), Policy{})
	tl.AddTrack(&TrackInfo{Number: 3, Frames: frames(3, 10)})
	tl.AddTrack(&TrackInfo{Number: 1, Frames: frames(1, 0)})
	tl.AddTrack(&TrackInfo{Number: 2, Frames: frames(2, 5)})
	tl.Repair()
	var got []uint64
	for n := range tl.Tracks {
		got = append(got, n)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("tracks=%v", got)
	}
}
