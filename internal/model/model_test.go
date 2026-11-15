package model

import (
	"math/big"
	"testing"
)

func u64(v uint64) *uint64 { return &v }

func TestExpandTimelinePrerollAndTail(t *testing.T) {
	tl := &SegmentTimeline{S: []*S{{T: u64(3003), D: 180180, R: 28}, {D: 174780}}}
	segs, probs := ExpandTimeline(tl, 1)
	if len(probs) != 0 {
		t.Fatalf("unexpected problems: %v", probs)
	}
	if len(segs) != 30 || segs[0].Number != 1 || segs[29].Number != 30 {
		t.Fatalf("bad numbering: n=%d first=%d last=%d", len(segs), segs[0].Number, segs[29].Number)
	}
	if segs[0].T != 3003 || segs[1].T != 3003+180180 {
		t.Fatalf("bad t: %d %d", segs[0].T, segs[1].T)
	}
	last := segs[29]
	if last.D != 174780 || last.T != 3003+29*180180 {
		t.Fatalf("bad tail: %+v", last)
	}
}

func TestExpandTimelineProblems(t *testing.T) {
	tl := &SegmentTimeline{S: []*S{
		{T: u64(0), D: 100},
		{T: u64(50), D: 100},  // regression
		{T: u64(500), D: 100}, // gap
		{D: 0},                // zero duration
	}}
	_, probs := ExpandTimeline(tl, 1)
	codes := map[string]bool{}
	for _, p := range probs {
		codes[p.Code] = true
	}
	for _, want := range []string{CodeTimeRegression, CodeMissingSegment, CodeZeroDuration} {
		if !codes[want] {
			t.Fatalf("missing %s in %v", want, probs)
		}
	}
}

func TestMergeRunsMergesAndSetsStart(t *testing.T) {
	tl := MergeRuns([]uint64{10, 10, 10, 4}, 3003)
	if len(tl.S) != 2 || tl.S[0].R != 2 || tl.S[0].D != 10 || *tl.S[0].T != 3003 {
		t.Fatalf("bad merge: %+v", tl.S)
	}
	if tl.S[1].D != 4 || tl.S[1].T != nil {
		t.Fatalf("bad tail: %+v", tl.S[1])
	}
}

func TestValidate(t *testing.T) {
	rep := func(id string, bw int64, ts uint64) *Representation {
		return &Representation{ID: id, Bandwidth: bw, Template: &SegmentTemplate{Timescale: ts}}
	}
	m := &MPD{Periods: []*Period{
		{Start: big.NewRat(10, 1), Sets: []*AdaptationSet{{Reps: []*Representation{rep("a", 1, 90000), rep("a", 1, 90000), rep("b", 0, 90000), rep("c", 1, 0)}}}},
		{Start: big.NewRat(5, 1)},
	}}
	codes := map[string]bool{}
	for _, p := range m.Validate() {
		codes[p.Code] = true
	}
	for _, want := range []string{CodeDupID, CodeBadBandwidth, CodeZeroTimescale, CodePeriodOrder} {
		if !codes[want] {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestAbsPresentationMSAndNegative(t *testing.T) {
	// period start 60s, t=180180 ticks, pto=3003, timescale 90000
	got := AbsPresentationMS(big.NewRat(60, 1), 180180, 3003, 90000)
	want := new(big.Rat).Add(big.NewRat(60000, 1), big.NewRat(177177, 90))
	if got.Cmp(want) != 0 {
		t.Fatalf("got %s want %s", got.RatString(), want.RatString())
	}
	neg := PresentationTicks(big.NewRat(100, 1), big.NewRat(3003, 1))
	if neg.Sign() >= 0 {
		t.Fatal("expected negative presentation time")
	}
	if ticks, exact := MSToTicksExact(2002, 90000); ticks != 180180 || !exact {
		t.Fatalf("MSToTicksExact: %d %v", ticks, exact)
	}
}
