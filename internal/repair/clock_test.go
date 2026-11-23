package repair

import (
	"errors"
	"math"
	"testing"

	"archive.local/archive/internal/model"
	mp4 "archive.local/archive/internal/mp4"
)

func tl(t *testing.T, trackID uint32, ts uint32, origin int64) *model.Timeline {
	t.Helper()
	out, err := model.NewTimeline(model.Timebase{TrackID: trackID, Timescale: ts, OriginTicks: origin})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// synthetic builds a track whose source timestamps contain the supplied
// deltas starting at origin.
func synthetic(trackID uint32, state model.Timebase, deltas []int64, syncEvery int) *mp4.Track {
	tr := &mp4.Track{TrackID: trackID, Timescale: state.Timescale, Origin: state.OriginTicks}
	src := state.OriginTicks
	for i, d := range deltas {
		tr.Samples = append(tr.Samples, mp4.Sample{
			Index: i, Size: uint32(100 + i), Offset: uint64(1000 + i*120),
			SourceTicks: src, Sync: syncEvery > 0 && i%syncEvery == 0,
		})
		src += d
	}
	for i := range tr.Samples {
		if tr.Samples[i].Sync {
			tr.Sync = append(tr.Sync, uint32(i+1))
		}
	}
	return tr
}

func TestRepairClockFixesRegression(t *testing.T) {
	deltas := []int64{3000, 3000, -2500, 3000, 3000}
	tr := synthetic(1, model.Timebase{TrackID: 1, Timescale: 90000}, deltas, 0)
	state := tl(t, 1, 90000, 0)
	got, rep, err := RepairClock(tr, state, 90*60)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if rep.Regressed != 1 {
		t.Fatalf("regressed = %d", rep.Regressed)
	}
	if got[3].SourceTicks != tr.Samples[3].SourceTicks {
		t.Fatalf("source ticks were rewritten: %d", got[3].SourceTicks)
	}
	if !got[3].Repaired {
		t.Fatal("regressed sample not marked repaired")
	}
	for i := 1; i < len(got); i++ {
		if got[i].Unified <= got[i-1].Unified {
			t.Fatalf("playback clock not increasing at %d: %d <= %d", i, got[i].Unified, got[i-1].Unified)
		}
	}
}

func TestRepairClockFixesZeroAndMissingDeltas(t *testing.T) {
	deltas := []int64{3000, 0, 0, 3000}
	tr := synthetic(1, model.Timebase{TrackID: 1, Timescale: 90000}, deltas, 0)
	got, rep, err := RepairClock(tr, state(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Missing != 2 {
		t.Fatalf("missing = %d", rep.Missing)
	}
	if got[1].Unified <= got[0].Unified || got[2].Unified <= got[1].Unified {
		t.Fatalf("zero delta not repaired: %v", []int64{got[0].Unified, got[1].Unified, got[2].Unified})
	}
	// Median of the positive deltas is 3000 media ticks; 3000/90000 s maps
	// to 33 333 µs on the unified clock.
	if got[1].Unified-got[0].Unified != 33_333 {
		t.Fatalf("repair delta = %d", got[1].Unified-got[0].Unified)
	}
}

func TestRepairClockPreservesNonZeroOrigin(t *testing.T) {
	tr := synthetic(9, model.Timebase{TrackID: 9, Timescale: 90000, OriginTicks: 324000000},
		[]int64{3000, 3000, 3000}, 0)
	got, _, err := RepairClock(tr, tl(t, 9, 90000, 324000000), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Unified != 3_600_000_000 {
		t.Fatalf("origin unified = %d", got[0].Unified)
	}
	// 90 kHz divides into microseconds exactly (x100/9), so frames stay
	// exact: two 3000-tick frames are 66 666 µs apart only if truncated.
	if got[2].Unified-got[0].Unified != 66_666 {
		t.Fatalf("two frames = %d", got[2].Unified-got[0].Unified)
	}
}

func TestRepairClockGapIsReportedNotHidden(t *testing.T) {
	tr := synthetic(1, model.Timebase{TrackID: 1, Timescale: 90000},
		[]int64{3000, 900000, 3000}, 0)
	_, rep, err := RepairClock(tr, state(t), 45000)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Gaps != 1 {
		t.Fatalf("gaps = %d", rep.Gaps)
	}
}

func state(t *testing.T) *model.Timeline { return tl(t, 1, 90000, 0) }

func TestRepairClockOverflowStopsAtFirstBadSample(t *testing.T) {
	// A 1 MHz clock is representable up to MaxInt64 ticks, so the overflow
	// only appears after the repaired clock crosses the boundary.
	tr := synthetic(1, model.Timebase{TrackID: 1, Timescale: 1_000_000},
		[]int64{1, 1, 1}, 0)
	tr.Samples[0].SourceTicks = math.MaxInt64 - 2
	_, rep, err := RepairClock(tr, tl(t, 1, 1_000_000, math.MaxInt64-2), 0)
	if err == nil {
		t.Fatal("expected overflow")
	}
	var me *model.Error
	if !errors.As(err, &me) || me.Recoverable == nil {
		t.Fatalf("err = %v", err)
	}
	if model.CodeOf(err) != model.CodeTimescaleOverflow {
		t.Fatalf("code = %q", model.CodeOf(err))
	}
	if me.Sample < 1 || me.Sample >= 3 {
		t.Fatalf("first bad sample = %d", me.Sample)
	}
	if me.Recoverable.LastSample != me.Sample {
		t.Fatalf("recoverable = %+v", me.Recoverable)
	}
	if rep.FirstError == nil {
		t.Fatal("report missing first error")
	}
}

func TestEmptyTrackHasNoSamples(t *testing.T) {
	tr := &mp4.Track{TrackID: 1}
	_, _, err := RepairClock(tr, tl(t, 1, 90000, 0), 0)
	if model.CodeOf(err) != model.CodeTruncatedSample {
		t.Fatalf("code = %q", model.CodeOf(err))
	}
}
