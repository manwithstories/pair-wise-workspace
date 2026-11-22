package fanin

import (
	"testing"

	"mediatool/internal/clock"
	"mediatool/internal/fanout"
)

func TestAlignClean(t *testing.T) {
	files := []fanout.SegmentFile{
		{RepID: 0, SegID: 0, Start: 0, End: 180000, FirstPTS: 0, LastPTS: 179640, Complete: true},
		{RepID: 1, SegID: 0, Start: 0, End: 180000, FirstPTS: 0, LastPTS: 179640, Complete: true},
	}
	res := Align(files, clock.MsToTicks(80))
	if res.Mismatches != 0 || len(res.FailedReps) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestAlignDeviationFailsSingleLayer(t *testing.T) {
	files := []fanout.SegmentFile{
		{RepID: 0, SegID: 0, Start: 0, End: 180000, FirstPTS: 0, LastPTS: 179640, Complete: true},
		{RepID: 1, SegID: 0, Start: 0, End: 180000, FirstPTS: 36000, LastPTS: 179640, Complete: true},
	}
	res := Align(files, clock.MsToTicks(80))
	if res.Mismatches != 1 {
		t.Fatalf("mismatches = %d, want 1", res.Mismatches)
	}
	if !res.FailedReps[1] || res.FailedReps[0] {
		t.Fatalf("failed reps = %v, want only rep 1", res.FailedReps)
	}
	if res.Faults[0].Kind != clock.FaultBoundaryMismatch {
		t.Fatalf("fault = %+v", res.Faults[0])
	}
}

func TestAlignSkipsIncomplete(t *testing.T) {
	files := []fanout.SegmentFile{
		{RepID: 0, SegID: 0, Start: 0, End: 180000, FirstPTS: 99999, Complete: false},
	}
	res := Align(files, clock.MsToTicks(80))
	if res.Mismatches != 0 {
		t.Fatalf("incomplete segments must be skipped: %+v", res)
	}
}
