package ladder

import (
	"testing"

	"example.com/medialab/archive/internal/model"
)

func TestParseBitrates(t *testing.T) {
	reps, err := ParseBitrates("128k, 64k")
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 2 || reps[0].ID != "64k" || reps[1].Bitrate != 128000 {
		t.Fatalf("reps = %+v", reps)
	}
	for _, bad := range []string{"", "64k", "64k,64k", "64k,abc", "64k,-8k", "64k,0k"} {
		if _, err := ParseBitrates(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestFrameDurationFor(t *testing.T) {
	if d, err := FrameDurationFor(48000, 960); err != nil || d != 20000 {
		t.Fatalf("opus frame = %d/%v", d, err)
	}
	if d, err := FrameDurationFor(48000, 1024); err != nil || d != 21333 {
		t.Fatalf("aac frame = %d/%v", d, err)
	}
	if _, err := FrameDurationFor(0, 1024); err == nil {
		t.Fatal("zero rate accepted")
	}
}

func TestGrid(t *testing.T) {
	g := Grid(25, 10)
	if len(g) != 3 || g[0] != 10 || g[1] != 20 || g[2] != 25 {
		t.Fatalf("grid = %v", g)
	}
	if len(Grid(10, 10)) != 1 || Grid(10, 10)[0] != 10 {
		t.Fatal("exact duration must have a single final boundary")
	}
	if Grid(0, 10) != nil {
		t.Fatal("zero duration must have no grid")
	}
}

func reps() []model.Representation {
	return []model.Representation{
		{ID: "64k", Bitrate: 64000, FrameDurationUS: 20000},
		{ID: "128k", Bitrate: 128000, FrameDurationUS: 20000},
	}
}

func TestAlignSharedBoundaries(t *testing.T) {
	edges := map[model.RepresentationID][]int64{
		"64k":  {20000, 40000, 60000, 80000, 100000},
		"128k": {20000, 40000, 60000, 80000, 100000},
	}
	ws, maxSkew, err := Align(reps(), edges, 100000, 40000)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 6 { // 3 windows x 2 reps
		t.Fatalf("windows = %d", len(ws))
	}
	// Non-final boundary 40000 is exactly on an edge, final 100000 too.
	// Boundary 80000 is exact; the 40000 first window boundary is exact.
	if maxSkew != 0 {
		t.Fatalf("maxSkew = %d", maxSkew)
	}
	for _, w := range ws {
		if w.Index == 2 && (w.StartUS != 80000 || w.EndUS != 100000 || !w.IsFinal) {
			t.Fatalf("final window = %+v", w)
		}
	}
}

func TestAlignRecordsSkewAndTrim(t *testing.T) {
	edges := map[model.RepresentationID][]int64{
		"64k":  {20000, 40000, 60000, 80000},
		"128k": {21000, 42000, 63000, 84000},
	}
	ws, maxSkew, err := Align(reps(), edges, 80000, 40000)
	if err != nil {
		t.Fatal(err)
	}
	if maxSkew != 4000 { // 128k edge 84000 vs final boundary 80000
		t.Fatalf("maxSkew = %d", maxSkew)
	}
	var final SegmentWindow
	for _, w := range ws {
		if w.Rep == "128k" && w.IsFinal {
			final = w
		}
	}
	if final.EdgeTrimUS != 4000 { // 84000 observed vs 80000 shared
		t.Fatalf("trim = %d", final.EdgeTrimUS)
	}
	if final.FrameCount != 2 {
		t.Fatalf("frame count = %d", final.FrameCount)
	}
}

func TestAlignRejectsBadInput(t *testing.T) {
	if _, _, err := Align(reps(), nil, 0, 1000); err == nil {
		t.Fatal("zero duration accepted")
	}
	if _, _, err := Align(reps(), nil, 1000, 0); err == nil {
		t.Fatal("zero segment accepted")
	}
	if _, _, err := Align(reps()[:1], nil, 1000, 1000); err == nil {
		t.Fatal("single representation accepted")
	}
}

func TestBoundaryDelta(t *testing.T) {
	segs := []model.Segment{
		{Rep: "64k", Index: 0, StartUS: 0},
		{Rep: "128k", Index: 0, StartUS: 0},
		{Rep: "64k", Index: 1, StartUS: 10000},
		{Rep: "128k", Index: 1, StartUS: 10000},
	}
	if d := BoundaryDelta(segs); d != 0 {
		t.Fatalf("delta = %d", d)
	}
	segs[3].StartUS = 10020
	if d := BoundaryDelta(segs); d != 20 {
		t.Fatalf("delta = %d", d)
	}
}
