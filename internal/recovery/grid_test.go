package recovery

import (
	"testing"

	"fieldcut/internal/media"
)

func TestPositionsDuplicateValidInsideInterval(t *testing.T) {
	// Two valid captures map to the same 25 fps interval: the later
	// sequence wins deterministically and no hole is invented.
	plan, err := AnalyzePositions("B", 4, []Position{
		{Seq: 1, Index: 1, Present: true, Valid: true},
		{Seq: 2, Index: 1, Present: true, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Index 1 has two valid captures; the leading hole (0) and trailing
	// run (2,3) lack a valid flank on one side, so nothing is eligible.
	if plan.Eligible != 0 || plan.Recovered != 0 {
		t.Fatalf("eligible=%d recovered=%d", plan.Eligible, plan.Recovered)
	}
	reasons := map[string]int{}
	for _, m := range plan.Missing {
		reasons[m.Reason]++
	}
	if reasons["boundary-before"] != 1 || reasons["boundary-after"] != 2 {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestPositionsHoleBetweenFramesAtDifferentIntervals(t *testing.T) {
	// A gap of exactly two empty indices is the largest interpolable one.
	plan, err := AnalyzePositions("B", 6, []Position{
		{Seq: 0, Index: 0, Present: true, Valid: true},
		{Seq: 5, Index: 5, Present: true, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Missing) != 4 {
		t.Fatalf("missing = %d, want 4", len(plan.Missing))
	}
	if plan.Eligible != 0 {
		t.Fatalf("eligible = %d, want 0", plan.Eligible)
	}
}

func TestPositionsCorruptBetweenValidSurvivesAsRepair(t *testing.T) {
	plan, err := AnalyzePositions("B", 3, []Position{
		{Seq: 0, Index: 0, Present: true, Valid: true},
		{Seq: 1, Index: 1, Present: true, Valid: false},
		{Seq: 2, Index: 2, Present: true, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.CorruptRepaired != 1 || plan.Eligible != 0 {
		t.Fatalf("corrupt=%d eligible=%d", plan.CorruptRepaired, plan.Eligible)
	}
	if plan.Rate() != 1.0 {
		t.Fatalf("rate = %v", plan.Rate())
	}
	w, ok := plan.InterpAt(1)
	if !ok || w.Weight.Cmp(media.NewRational(1, 2)) != 0 {
		t.Fatalf("weight = %v ok=%v", w.Weight, ok)
	}
}
