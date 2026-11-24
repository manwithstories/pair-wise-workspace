package core

import (
	"testing"

	mp4adapter "media-check/internal/adapter/mp4"
	"media-check/internal/model"
)

func frames(n, gop int, delta uint32) []model.Frame {
	out := make([]model.Frame, n)
	for i := range out {
		out[i] = model.Frame{
			Seq: uint32(1000 + i), Time: int64(i) * int64(delta),
			Keyframe: i%gop == 0,
			Color:    model.ColorMeta{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
			Payload:  []byte{byte(i), byte(i >> 8)},
		}
	}
	return out
}

func TestInspectKeyframeGaps(t *testing.T) {
	rep := Inspect(frames(90, 30, 3000), 30)
	if len(rep.KeyframeIdx) != 3 || rep.MaxGap != 30 {
		t.Fatalf("keyframes=%v maxGap=%d", rep.KeyframeIdx, rep.MaxGap)
	}
	if len(rep.Faults) != 0 {
		t.Fatalf("clean tagged input raised faults: %v", rep.Faults)
	}
}

func TestInspectColorDecisions(t *testing.T) {
	fs := frames(10, 5, 3000)
	fs[3].Color = model.ColorMeta{} // missing → carry forward
	fs[7].Color = model.ColorMeta{Primaries: "bt2020", Transfer: "pq", Matrix: "bt2020nc"}
	rep := Inspect(fs, 5)
	if !rep.Faults.HasCode(model.FaultColorMissing) {
		t.Fatal("expected color_missing")
	}
	if !rep.Faults.HasCode(model.FaultColorConflict) {
		t.Fatal("expected color_conflict")
	}
	var carry, conflict bool
	for _, d := range rep.ColorDecisions {
		if d.Action == "carry-forward" && d.After.Primaries == "bt709" {
			carry = true
		}
		if d.Action == "conflict-keep-first" && d.Before.Primaries == "bt2020" {
			conflict = true
		}
	}
	if !carry || !conflict {
		t.Fatalf("decisions not traceable: %+v", rep.ColorDecisions)
	}
	// source tags must not be overwritten in the frames themselves
	if fs[3].Color.Specified() {
		t.Fatal("source frame colour was silently overwritten")
	}
}

func TestBuildPlanCorruptRanges(t *testing.T) {
	ranges := []model.TimeRange{{Start: 0, End: 10}, {Start: 10, End: 20}, {Start: 20, End: 30}}
	faults := model.Faults{
		{Code: model.FaultHashMismatch, Segment: 1},
		{Code: model.FaultColorMissing, Segment: 2}, // metadata-only: not corrupt
	}
	plan := BuildPlan(ranges, faults)
	if plan.CorruptRanges != 1 {
		t.Fatalf("corrupt ranges=%d", plan.CorruptRanges)
	}
	if plan.Segments[1].Kind != SegmentCorrupt || plan.Segments[2].Kind != SegmentValid {
		t.Fatalf("plan wrong: %+v", plan.Segments)
	}
}

func TestRemuxSegmentRoundtrip(t *testing.T) {
	fs := frames(12, 4, 3000)
	for i := range fs {
		fs[i].Hash = hashOf(fs[i].Payload)
	}
	res, err := RemuxSegment(mp4adapter.Adapter{}, fs, 0, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if !res.RoundtripOK {
		t.Fatal("roundtrip failed on valid segment")
	}
	if res.Range != (model.TimeRange{Start: 0, End: 12 * 3000}) {
		t.Fatalf("range %+v", res.Range)
	}
	if res.DstHash == ([32]byte{}) {
		t.Fatal("dst hash not computed")
	}
}
