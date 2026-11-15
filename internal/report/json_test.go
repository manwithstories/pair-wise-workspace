package report

import (
	"testing"

	"rcctl/internal/plan"
	"rcctl/internal/state"
)

func TestBuildAndVerify(t *testing.T) {
	p := plan.New("news", 8, 32, 32, 30, 4, 400, 1)
	p.PlanID = p.ComputeID()
	results := []state.SegmentResult{
		{Index: 0, Frames: 4, TargetKbps: 400, ActualKbps: 390, DevPct: -2.5, PSNRDB: 35, Status: state.Done},
		{Index: 1, Frames: 4, TargetKbps: 400, ActualKbps: 380, DevPct: -5, PSNRDB: 34, Status: state.Done},
	}
	rep := Build(p, results, 1)
	if rep.Summary.Completed != 2 || rep.ResumedSegments != 1 || rep.Summary.MeanPSNRDB != 34.5 {
		t.Fatalf("unexpected summary: %#v", rep.Summary)
	}
	if got := Verify(rep, 5, 1.5); len(got) != 0 {
		t.Fatalf("unexpected violations at boundary: %#v", got)
	}
	if got := Verify(rep, 1, 0); len(got) == 0 {
		t.Fatal("expected threshold violations")
	}
}
