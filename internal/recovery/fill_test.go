package recovery

import (
	"math"
	"testing"

	"fieldcut/internal/ingest"
	"fieldcut/internal/media"
)

func entry(track string, seq int64) ingest.Entry {
	return ingest.Entry{Track: track, Seq: seq}
}

func allValid(ingest.Entry) bool { return true }

func holes(total int, holeSeqs ...int64) []ingest.Entry {
	var out []ingest.Entry
	for i := int64(0); i < int64(total); i++ {
		out = append(out, entry("A", i))
	}
	skip := map[int64]bool{}
	for _, h := range holeSeqs {
		skip[h] = true
	}
	filtered := out[:0]
	for _, e := range out {
		if !skip[e.Seq] {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func TestSingleHoleRecovered(t *testing.T) {
	plan, err := Analyze("A", 8, holes(8, 3), allValid)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if plan.Eligible != 1 || plan.Recovered != 1 {
		t.Fatalf("eligible=%d recovered=%d", plan.Eligible, plan.Recovered)
	}
	in, ok := plan.InterpAt(3)
	if !ok {
		t.Fatal("no interpolation planned")
	}
	if in.Before != 2 || in.After != 4 {
		t.Fatalf("flanks = %d,%d", in.Before, in.After)
	}
	if in.Weight.Cmp(media.NewRational(1, 2)) != 0 {
		t.Fatalf("weight = %v, want 1/2", in.Weight)
	}
	if plan.Rate() != 1.0 {
		t.Fatalf("rate = %v", plan.Rate())
	}
}

func TestTwoHolesRecoveredWithExactWeights(t *testing.T) {
	plan, err := Analyze("A", 8, holes(8, 4, 5), allValid)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Interps) != 2 {
		t.Fatalf("interps = %d, want 2", len(plan.Interps))
	}
	if plan.Interps[0].Weight.Cmp(media.NewRational(1, 3)) != 0 ||
		plan.Interps[1].Weight.Cmp(media.NewRational(2, 3)) != 0 {
		t.Fatalf("weights = %v %v", plan.Interps[0].Weight, plan.Interps[1].Weight)
	}
}

func TestLongGapIsExplicitlyMissing(t *testing.T) {
	plan, err := Analyze("A", 12, holes(12, 3, 4, 5), allValid)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Recovered != 0 || len(plan.Missing) != 3 {
		t.Fatalf("recovered=%d missing=%d", plan.Recovered, len(plan.Missing))
	}
	for _, m := range plan.Missing {
		if m.Reason != "gap-too-long" {
			t.Fatalf("reason = %q", m.Reason)
		}
	}
}

func TestBoundaryHolesAreNeverRecovered(t *testing.T) {
	plan, err := Analyze("A", 8, holes(8, 0, 7), allValid)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Eligible != 0 || plan.Recovered != 0 {
		t.Fatalf("eligible=%d recovered=%d, want 0/0", plan.Eligible, plan.Recovered)
	}
	reasons := map[string]bool{}
	for _, m := range plan.Missing {
		reasons[m.Reason] = true
	}
	if !reasons["boundary-before"] || !reasons["boundary-after"] {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestCorruptFramesRepairedButNotEligible(t *testing.T) {
	present := holes(8)
	valid := func(e ingest.Entry) bool { return e.Seq != 2 }
	plan, err := Analyze("A", 8, present, valid)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CorruptRepaired != 1 || len(plan.Interps) != 1 {
		t.Fatalf("corrupt_repaired=%d interps=%d", plan.CorruptRepaired, len(plan.Interps))
	}
	if plan.Eligible != 0 || plan.Recovered != 0 {
		t.Fatalf("rate denominator must ignore corrupt frames: eligible=%d recovered=%d", plan.Eligible, plan.Recovered)
	}
	if len(plan.Missing) != 0 {
		t.Fatalf("missing = %v", plan.Missing)
	}
}

func TestCorruptRunWithHoleCountsOnlyTheHole(t *testing.T) {
	present := holes(8, 3)
	valid := func(e ingest.Entry) bool { return e.Seq != 2 }
	plan, err := Analyze("A", 8, present, valid)
	if err != nil {
		t.Fatal(err)
	}
	// Run [2,4): 2 is corrupt, 3 was never delivered; both are synthesized,
	// but only the injected hole enters the rate denominator.
	if plan.CorruptRepaired != 1 || plan.Eligible != 1 || plan.Recovered != 1 {
		t.Fatalf("corrupt=%d eligible=%d recovered=%d", plan.CorruptRepaired, plan.Eligible, plan.Recovered)
	}
}

func TestCorruptAtBoundaryIsExplicitMissing(t *testing.T) {
	present := holes(8)
	valid := func(e ingest.Entry) bool { return e.Seq != 0 }
	plan, err := Analyze("A", 8, present, valid)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CorruptRepaired != 0 || len(plan.Missing) != 1 || plan.Missing[0].Reason != "corrupt" {
		t.Fatalf("corrupt=%d missing=%v", plan.CorruptRepaired, plan.Missing)
	}
}

func TestDedupeKeepsFirstArrival(t *testing.T) {
	got := Dedupe([]ingest.Entry{entry("A", 2), entry("A", 0), entry("A", 2), entry("A", 1)})
	if len(got) != 3 || got[0].Seq != 0 || got[1].Seq != 1 || got[2].Seq != 2 {
		t.Fatalf("dedupe = %+v", got)
	}
}

func TestDuplicatesDoNotInflateEligibility(t *testing.T) {
	present := append(holes(8, 3), entry("A", 1), entry("A", 1))
	plan, err := Analyze("A", 8, present, allValid)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Eligible != 1 || plan.Recovered != 1 {
		t.Fatalf("eligible=%d recovered=%d", plan.Eligible, plan.Recovered)
	}
}

func TestRateDenominatorIsInjectedHoles(t *testing.T) {
	// Three recoverable single holes -> denominator 3.
	present := holes(12, 2, 5, 9)
	plan, err := Analyze("A", 12, present, allValid)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Eligible != 3 || plan.Recovered != 3 {
		t.Fatalf("eligible=%d recovered=%d", plan.Eligible, plan.Recovered)
	}
	if math.Abs(plan.Rate()-1.0) > 1e-12 {
		t.Fatalf("rate = %v", plan.Rate())
	}
}
