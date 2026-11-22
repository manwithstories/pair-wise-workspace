package reorder

import (
	"testing"

	"mediaroute/internal/fault"
)

func TestRecoveryBurstClassification(t *testing.T) {
	var r Recovery
	r.Record("v", Gap{From: 10, To: 16}) // 7 lost = burst
	r.Record("v", Gap{From: 30, To: 31}) // 2 lost = isolated
	if r.BurstsRecovered != 1 {
		t.Fatalf("bursts = %d, want 1", r.BurstsRecovered)
	}
	if len(r.Gaps) != 2 || r.Gaps[0].Count() != 7 || r.Gaps[1].Track != "v" {
		t.Fatalf("gaps = %+v", r.Gaps)
	}
	faults := r.Faults()
	if len(faults) != 2 || faults[0].Category != fault.CatLoss || faults[0].Track != "v" {
		t.Fatalf("faults = %+v", faults)
	}
}

func TestWindowGapsFeedRecovery(t *testing.T) {
	w := NewWindow(4)
	w.Push(ev(0))
	w.Push(ev(2))
	w.Push(ev(50)) // overflow: gap 1..1 and 3..49
	var r Recovery
	r.RecordAll("v", w.Gaps)
	if r.BurstsRecovered != 1 { // 3..49 is a burst
		t.Fatalf("bursts = %d, want 1", r.BurstsRecovered)
	}
	if len(r.Gaps) != 2 {
		t.Fatalf("gaps = %+v", r.Gaps)
	}
}
