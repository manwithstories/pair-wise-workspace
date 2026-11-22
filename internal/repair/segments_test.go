package repair

import (
	"testing"

	"mediacheck/internal/model"
)

func fault(id int, kind model.Kind, track string, start, end int64, rec model.Recovery) model.Fault {
	return model.Fault{
		ID: id, Kind: kind, Severity: model.SeverityError,
		Track: track, StartMs: start, EndMs: end, Recovery: rec,
	}
}

func TestAdjudicateRecoveredKeptAndDamageQuarantined(t *testing.T) {
	faults := []model.Fault{
		fault(1, model.KindRTPGap, "video", 100, 200, model.RecRecovered),
		fault(2, model.KindCorruptSample, "video", 500, 700, model.RecPending),
	}
	ends := map[string]int64{"video": 1000}
	res := Adjudicate(faults, ends, DefaultPolicy())
	if len(res.Segments) != 2 {
		t.Fatalf("want 2 segments, got %d", len(res.Segments))
	}
	if res.Segments[0].Recovery != model.RecRecovered {
		t.Fatal("recoverable RTP gap must stay recovered")
	}
	if res.Segments[1].Recovery != model.RecQuarantined {
		t.Fatal("structural damage must be quarantined")
	}
}

func TestAdjudicateMergesNearbyFaults(t *testing.T) {
	pol := Policy{MergeGapMs: 250, MaxQuarantineRatio: 1.0}
	faults := []model.Fault{
		fault(1, model.KindCorruptSample, "v", 100, 200, model.RecPending),
		fault(2, model.KindCorruptSample, "v", 300, 400, model.RecPending), // 100ms gap
	}
	res := Adjudicate(faults, map[string]int64{"v": 1000}, pol)
	if len(res.Segments) != 1 {
		t.Fatalf("nearby faults should merge into 1 segment, got %d", len(res.Segments))
	}
	if res.Segments[0].StartMs != 100 || res.Segments[0].EndMs != 400 {
		t.Fatalf("merged span wrong: %+v", res.Segments[0])
	}
	if len(res.Segments[0].FaultIDs) != 2 {
		t.Fatal("merged segment must retain both fault ids")
	}
}

func TestAdjudicateDeterministic(t *testing.T) {
	faults := []model.Fault{
		fault(2, model.KindCorruptSample, "v", 500, 600, model.RecPending),
		fault(1, model.KindCorruptSample, "v", 100, 200, model.RecPending),
	}
	pol := DefaultPolicy()
	a := Adjudicate(faults, map[string]int64{"v": 1000}, pol)
	b := Adjudicate(faults, map[string]int64{"v": 1000}, pol)
	if len(a.Segments) != len(b.Segments) {
		t.Fatal("segment count differs across runs")
	}
	for i := range a.Segments {
		if a.Segments[i].StartMs != b.Segments[i].StartMs ||
			a.Segments[i].Recovery != b.Segments[i].Recovery {
			t.Fatal("segment adjudication not deterministic")
		}
	}
}

func TestQuarantineBudgetFlipsToUnrecovered(t *testing.T) {
	// A fault covering 80% of a 1000ms track exceeds the 0.5 ratio.
	faults := []model.Fault{
		fault(1, model.KindCorruptSample, "v", 0, 800, model.RecPending),
	}
	res := Adjudicate(faults, map[string]int64{"v": 1000}, DefaultPolicy())
	if !res.HasUnrecovered() {
		t.Fatal("over-budget quarantine must be marked unrecovered")
	}
}

func TestCommittedBoundarySkipsQuarantine(t *testing.T) {
	faults := []model.Fault{
		fault(1, model.KindCorruptSample, "v", 300, 500, model.RecPending),
	}
	res := Adjudicate(faults, map[string]int64{"v": 1000}, DefaultPolicy())
	if len(res.Boundaries) == 0 {
		t.Fatal("expected committed boundaries")
	}
	last := res.Boundaries[len(res.Boundaries)-1]
	if last.EndMs != 1000 {
		t.Fatalf("final boundary must reach track end, got %d", last.EndMs)
	}
}
