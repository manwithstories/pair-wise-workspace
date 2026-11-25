package timeline

import (
	"testing"

	"mediad/internal/clock"
	"mediad/internal/codec"
)

func unit(start, dur int64) codec.AccessUnit {
	return codec.AccessUnit{
		Start: clock.At(start, clock.Base48k),
		Dur:   clock.At(dur, clock.Base48k),
	}
}

func TestUniformGridAcrossTracks(t *testing.T) {
	tracks := []Track{
		{ID: "studio", Units: []codec.AccessUnit{unit(0, 48000), unit(48000, 48000)}},
		{ID: "field", Units: []codec.AccessUnit{unit(0, 96000)}},
	}
	segs := Segmentize(tracks, clock.At(48000, clock.Base48k), clock.Base48k)
	if len(segs) != 2 {
		t.Fatalf("segments=%d, want 2", len(segs))
	}
	for i, s := range segs {
		if s.Start.Ticks != int64(i)*48000 || s.End.Ticks != int64(i+1)*48000 {
			t.Fatalf("seg %d bounds %v..%v", i, s.Start, s.End)
		}
		if len(s.Units["studio"]) != 1 || len(s.Units["field"]) != 1 {
			t.Fatalf("seg %d units %+v", i, s.Units)
		}
	}
}

func TestExplicitGapNotFabricated(t *testing.T) {
	tracks := []Track{
		{ID: "remote", Units: []codec.AccessUnit{unit(0, 960), unit(48000, 960)}},
	}
	segs := Segmentize(tracks, clock.At(48000, clock.Base48k), clock.Base48k)
	if len(segs) != 2 {
		t.Fatalf("segments=%d, want 2", len(segs))
	}
	g := segs[0].Gaps // the hole 960..48000 lies inside segment 0
	if len(g) != 1 || g[0].Start.Ticks != 960 || g[0].End.Ticks != 48000 {
		t.Fatalf("gaps=%+v, want 960..48000", g)
	}
	if c := Coverage(segs[0], "remote"); c != 960.0/48000.0 {
		t.Fatalf("coverage=%f, want %f", c, 960.0/48000.0)
	}
}

func TestZeroDurationAndOutOfRange(t *testing.T) {
	zero := unit(24000, 0)                       // zero-duration keep-alive
	oob := unit(-100, 960)                       // out-of-range timestamp
	tracks := []Track{{ID: "t", Units: []codec.AccessUnit{zero, oob, unit(47000, 960)}}}
	segs := Segmentize(tracks, clock.At(48000, clock.Base48k), clock.Base48k)
	if len(segs[0].Units["t"]) != 3 {
		t.Fatalf("units=%d, want 3 (zero-duration kept, negative clamped)",
			len(segs[0].Units["t"]))
	}
}

func TestCrossTrackBoundaryAlignment(t *testing.T) {
	// 44.1k-based track must land on the same 48k grid.
	u := codec.AccessUnit{
		Start: clock.At(44100, clock.Base44k), // exactly 1s
		Dur:   clock.At(44100, clock.Base44k),
	}
	tracks := []Track{{ID: "aac", Units: []codec.AccessUnit{u}}}
	segs := Segmentize(tracks, clock.At(48000, clock.Base48k), clock.Base48k)
	if len(segs) != 2 {
		t.Fatalf("segments=%d, want 2", len(segs))
	}
	if len(segs[1].Units["aac"]) != 1 {
		t.Fatal("1s unit must fall in segment 1 on the shared grid")
	}
}
