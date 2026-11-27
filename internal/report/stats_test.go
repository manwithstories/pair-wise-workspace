package report

import (
	"testing"

	"aircheck/internal/fault"
	"aircheck/internal/plan"
	"aircheck/internal/scan"
)

func TestFinalizePass(t *testing.T) {
	r := &Report{
		Package: "p",
		Renditions: []RenditionStats{
			{Name: "a", TargetBPS: 1000000, ActualBPS: 1040000, DriftPct: 4.0, Frames: 100},
		},
		MaxAlignmentMs: 3.5, EventsTotal: 5,
	}
	r.Finalize()
	if !r.OK || len(r.Faults) != 0 || r.Digest == "" {
		t.Fatalf("want pass, got %+v", r)
	}
}

func TestFinalizeGates(t *testing.T) {
	r := &Report{
		Renditions: []RenditionStats{
			{Name: "a", TargetBPS: 1000000, ActualBPS: 1200000, DriftPct: 20, Frames: 0},
		},
		MaxAlignmentMs: 55, EventMissing: 2, EventsTotal: 4,
	}
	r.Finalize()
	if r.OK {
		t.Fatal("want fail")
	}
	codes := map[fault.Code]bool{}
	for _, f := range r.Faults {
		codes[f.Code] = true
	}
	for _, want := range []fault.Code{fault.CodeDriftExceeded, fault.CodeMissingTrack, fault.CodeAlignExceeded, fault.CodeEventMissing} {
		if !codes[want] {
			t.Fatalf("missing fault %s in %+v", want, r.Faults)
		}
	}
}

func TestDigestDeterministic(t *testing.T) {
	mk := func() *Report {
		r := &Report{Package: "p", Renditions: []RenditionStats{{Name: "a", Frames: 10}}, EventsTotal: 1}
		r.Finalize()
		return r
	}
	if mk().Digest != mk().Digest {
		t.Fatal("digest not deterministic")
	}
}

func TestEventCoverage(t *testing.T) {
	cp := &plan.CutPlan{Events: []scan.Event{
		{Tick: 1000}, {Tick: 200000}, {Tick: 400000},
	}}
	total, missing := EventCoverage(cp, 300000)
	if total != 3 || missing != 1 {
		t.Fatalf("total=%d missing=%d", total, missing)
	}
}
