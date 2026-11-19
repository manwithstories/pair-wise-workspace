package ladder

import (
	"testing"

	"mediapack/internal/clock"
	"mediapack/internal/demux"
	"mediapack/internal/segment"
)

func trackWithKeyframes(id uint32, kfTicks ...int64) *demux.Track {
	tr := &demux.Track{ID: id, Kind: "video", Timescale: 90000}
	for i, t := range kfTicks {
		tr.Samples = append(tr.Samples, demux.Sample{
			Index: uint32(i), DTS: t, PTS: t, Duration: 3000,
			Size: 100, Keyframe: true, Available: true,
		})
	}
	return tr
}

func TestAlignExact(t *testing.T) {
	master := trackWithKeyframes(1, 0, 90000, 180000)
	rep := trackWithKeyframes(2, 0, 90000, 180000)
	bounds := []segment.Boundary{{Ticks: 0}, {Ticks: 90000}, {Ticks: 180000}}
	a := Align([]*demux.Track{master, rep}, bounds, clock.TicksFromMillis(80))
	if a.MaxErrorTicks != 0 {
		t.Fatalf("max error: %d", a.MaxErrorTicks)
	}
	if len(a.Missing) != 0 {
		t.Fatalf("missing: %v", a.Missing)
	}
	for _, ra := range a.Reps {
		for i := range ra.Aligned {
			if !ra.Aligned[i] {
				t.Fatalf("track %d boundary %d not aligned", ra.TrackID, i)
			}
		}
	}
}

func TestAlignWithinTolerance(t *testing.T) {
	master := trackWithKeyframes(1, 0, 90000, 180000)
	// Rep keyframes 40 ms (3600 ticks) after the master boundaries.
	rep := trackWithKeyframes(2, 3600, 93600, 183600)
	bounds := []segment.Boundary{{Ticks: 0}, {Ticks: 90000}, {Ticks: 180000}}
	a := Align([]*demux.Track{master, rep}, bounds, clock.TicksFromMillis(80))
	if a.MaxErrorTicks != 3600 {
		t.Fatalf("max error: %d", a.MaxErrorTicks)
	}
	if len(a.Missing) != 0 {
		t.Fatalf("missing: %v", a.Missing)
	}
}

func TestAlignBeyondToleranceReportsGap(t *testing.T) {
	master := trackWithKeyframes(1, 0, 90000, 180000)
	// Rep has no keyframe within 80 ms of the 90000 boundary.
	rep := trackWithKeyframes(2, 0, 180000)
	bounds := []segment.Boundary{{Ticks: 0}, {Ticks: 90000}, {Ticks: 180000}}
	a := Align([]*demux.Track{master, rep}, bounds, clock.TicksFromMillis(80))
	if len(a.Missing) != 1 {
		t.Fatalf("missing: %v", a.Missing)
	}
	if a.Missing[0].StartTicks != 90000 {
		t.Fatalf("missing start: %d", a.Missing[0].StartTicks)
	}
	if a.Reps[1].Aligned[1] {
		t.Fatal("boundary 1 must be flagged unaligned")
	}
	// Unaligned boundaries keep the master time so numbering stays stable.
	if a.Reps[1].CutTicks[1] != 90000 {
		t.Fatalf("cut ticks: %d", a.Reps[1].CutTicks[1])
	}
}

func TestAlignSkipsUnavailableKeyframes(t *testing.T) {
	master := trackWithKeyframes(1, 0, 90000)
	rep := trackWithKeyframes(2, 0, 90000)
	rep.Samples[1].Available = false
	bounds := []segment.Boundary{{Ticks: 0}, {Ticks: 90000}}
	a := Align([]*demux.Track{master, rep}, bounds, clock.TicksFromMillis(80))
	if len(a.Missing) != 1 {
		t.Fatalf("missing: %v", a.Missing)
	}
}
