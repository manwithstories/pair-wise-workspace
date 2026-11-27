package fanout

import (
	"testing"

	"tscheck/internal/analyze"
	"tscheck/internal/fixture"
	"tscheck/internal/model"
)

func TestRunThreeBranchesAligned(t *testing.T) {
	var branches []Branch
	for i := 0; i < 3; i++ {
		data, _ := fixture.Generate(fixture.Options{Seed: 41, Bytes: 300_000, Branch: i})
		branches = append(branches, Branch{Name: string(rune('a' + i)), Data: data})
	}
	results := Run(branches)
	if len(results) != 3 {
		t.Fatalf("results = %d", len(results))
	}
	for _, r := range results {
		if r.GOP.Keyframes < 2 {
			t.Fatalf("branch %s keyframes = %d", r.Name, r.GOP.Keyframes)
		}
		if len(r.Faults) != 0 {
			t.Fatalf("branch %s faults = %+v", r.Name, r.Faults)
		}
	}
	rep := MergeBoundaries(results)
	if rep.MaxDelta != 0 {
		t.Fatalf("max delta = %d", rep.MaxDelta)
	}
	if len(rep.Conflicts) != 0 {
		t.Fatalf("conflicts = %v", rep.Conflicts)
	}
}

func TestProcessDamagedBranch(t *testing.T) {
	data, _ := fixture.Generate(fixture.Options{Seed: 41, Bytes: 300_000, Branch: 0,
		Faults: fixture.FaultSpec{PCRWrap: true, CCJump: true, BadPSICRC: true, Truncate: true}})
	res := Process(Branch{Name: "damaged", Data: data})
	if len(res.Faults) == 0 {
		t.Fatal("damaged stream produced no faults")
	}
	// PCR wrap must be unwrapped: keyframes still monotonic and anchored.
	kf := res.GOP.KeyframeTicks
	for i := 1; i < len(kf); i++ {
		if kf[i] <= kf[i-1] {
			t.Fatalf("keyframes not monotonic: %v", kf)
		}
	}
	// Trusted packet list must exclude the fault offsets.
	if len(res.Trusted) == 0 || len(res.Trusted) >= res.Packets {
		t.Fatalf("trusted = %d packets = %d", len(res.Trusted), res.Packets)
	}
}

func TestMergeDetectsConflict(t *testing.T) {
	mk := func(name string, kfs ...model.Ticks) *BranchResult {
		return &BranchResult{Name: name, GOP: analyze.GOPStats{KeyframeTicks: kfs}}
	}
	aligned := MergeBoundaries([]*BranchResult{
		mk("a", 0, 900000), mk("b", 0, 900000)})
	if aligned.MaxDelta != 0 || len(aligned.Conflicts) != 0 {
		t.Fatalf("aligned = %+v", aligned)
	}
	shifted := MergeBoundaries([]*BranchResult{
		mk("a", 0, 900000), mk("b", 0, 900000+2*model.FrameTicks)})
	if len(shifted.Conflicts) != 1 || shifted.MaxDelta != 2*model.FrameTicks {
		t.Fatalf("shifted = %+v", shifted)
	}
	mismatch := MergeBoundaries([]*BranchResult{
		mk("a", 0, 900000), mk("b", 0)})
	if len(mismatch.Conflicts) != 1 {
		t.Fatalf("count mismatch = %+v", mismatch)
	}
}
