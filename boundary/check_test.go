package boundary

import (
	"testing"

	"medcheck/fault"
	"medcheck/index"
	"medcheck/subtitle"
)

func testIndex() *index.Index {
	return &index.Index{Version: index.IndexVersion, Segments: []index.Segment{
		{Index: 0, StartMS: 0, EndMS: 2000, Hash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{Index: 1, StartMS: 2000, EndMS: 4000, Hash: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"},
	}}
}

func TestDamagedSegmentAndCrossingCueAreUncuttable(t *testing.T) {
	fm := fault.NewModel()
	fm.Add(fault.Fault{Kind: fault.BadSegment, Segment: 1, Span: fault.Span{StartMS: 2500, EndMS: 3000}})
	doc := &subtitle.Document{Cues: []subtitle.Cue{
		{SourceLine: 8, StartMS: 2600, EndMS: 3200, Text: "half sentence"},
	}}
	layers := []LayerState{{Name: "1x", Hashes: map[int]string{
		0: testIndex().Segments[0].Hash, 1: testIndex().Segments[1].Hash,
	}, BadSegments: []int{1}}}
	rep := Check(testIndex(), doc, layers, fm)
	if rep.ConflictCount == 0 || rep.HashOK {
		t.Fatalf("expected conflicts and hash failure due bad layer: %+v", rep)
	}
	found := false
	for _, in := range rep.Uncuttable {
		if in.SourceLine == 8 && in.StartMS == 2600 && in.EndMS == 3200 {
			found = true
		}
	}
	if !found {
		t.Fatalf("cue propagation interval missing: %+v", rep.Uncuttable)
	}
}

func TestCueCrossingGOPBoundaryBlocksCutPoint(t *testing.T) {
	fm := fault.NewModel()
	doc := &subtitle.Document{Cues: []subtitle.Cue{{SourceLine: 3, StartMS: 1500, EndMS: 2500, Text: "cross"}}}
	layers := []LayerState{{Name: "1x", Hashes: map[int]string{
		0: testIndex().Segments[0].Hash, 1: testIndex().Segments[1].Hash,
	}}}
	rep := Check(testIndex(), doc, layers, fm)
	if rep.IsCuttable(2000) {
		t.Fatal("GOP boundary inside cue must not be cuttable")
	}
	if len(rep.CuttableBoundaries) != 0 {
		t.Fatalf("no other boundaries expected: %v", rep.CuttableBoundaries)
	}
}
