package pipeline

import "testing"

func TestEditPointsCutSampleExact(t *testing.T) {
	tl, plan, err := buildGraphPlan()
	if err != nil {
		t.Fatal(err)
	}
	blocks, edits, err := applyEditPoints(tl, plan)
	if err != nil {
		t.Fatal(err)
	}
	// The voice segment declares two edit points.
	if len(edits) != 2 {
		t.Fatalf("edits=%d want 2", len(edits))
	}
	for _, e := range edits {
		if e.Policy != EditPolicyPadSilence {
			t.Fatalf("policy=%q", e.Policy)
		}
		if e.SyncErrorMs < 0 || e.SyncErrorMs > 30 {
			t.Fatalf("sync error out of range: %.3f", e.SyncErrorMs)
		}
	}
	for i, b := range blocks {
		// The first real sample of the block is the next whole sample after
		// the cut, and exactly that sample (if any) is silence padding.
		realStart := b.Start + int64(b.Silenced)
		if realStart != edits[i].CutSample {
			t.Fatalf("block %d real start=%d cut sample=%d", i, realStart, edits[i].CutSample)
		}
		if b.Silenced != edits[i].PaddingSamples {
			t.Fatalf("block %d padding=%d edit says %d", i, b.Silenced, edits[i].PaddingSamples)
		}
		if edits[i].PaddingSamples != 0 && edits[i].PaddingSamples != 1 {
			t.Fatalf("padding must be 0 or 1 sample, got %d", edits[i].PaddingSamples)
		}
	}
}

func TestEditCutsAreStableAcrossRuns(t *testing.T) {
	tl, plan, _ := buildGraphPlan()
	_, a, err := applyEditPoints(tl, plan)
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := applyEditPoints(tl, plan)
	if err != nil {
		t.Fatal(err)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("cut %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}
