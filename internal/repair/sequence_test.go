package repair

import (
	"testing"

	"archivecheck/internal/fault"
)

func item(seg string, seq uint32, has bool, idx int) Item {
	return Item{Segment: seg, Sequence: seq, HasMfhd: has, Index: idx}
}

func TestOrderRepairsGapAndOrder(t *testing.T) {
	ord, err := Order([]Item{
		item("f3", 3, true, 0),
		item("f1", 1, true, 1),
		item("f2", 2, true, 2),
		item("f5", 5, true, 3),
		item("f6", 6, true, 4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ord.Items) != 5 {
		t.Fatalf("items = %d", len(ord.Items))
	}
	want := []string{"f1", "f2", "f3", "f5", "f6"}
	for i, it := range ord.Items {
		if it.Segment != want[i] {
			t.Fatalf("item %d = %s, want %s", i, it.Segment, want[i])
		}
	}
	if ord.Missing != 1 {
		t.Fatalf("missing = %d, want 1", ord.Missing)
	}
	var outOfOrder int
	for _, is := range ord.Issues {
		if is.Kind == "out_of_order" {
			outOfOrder++
		}
	}
	if outOfOrder != 3 {
		t.Fatalf("out_of_order = %d, want 3", outOfOrder)
	}
}

func TestOrderMissingMfhdKeepsManifestSlot(t *testing.T) {
	ord, err := Order([]Item{item("a", 1, true, 0), item("b", 0, false, 1), item("c", 2, true, 2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ord.Items) != 3 {
		t.Fatalf("items = %d", len(ord.Items))
	}
	var missing int
	for _, is := range ord.Issues {
		if is.Kind == "missing" {
			missing++
		}
	}
	if missing != 1 {
		t.Fatalf("missing issues = %d, want 1", missing)
	}
}

func TestOrderRejectsConflictingDuplicate(t *testing.T) {
	_, err := Order([]Item{item("a", 1, true, 0), item("b", 1, true, 1)})
	if err == nil || !fault.IsKind(err, fault.KindSequence) {
		t.Fatalf("err = %v", err)
	}
}

func TestOrderDropsIdenticalDuplicate(t *testing.T) {
	ord, err := Order([]Item{item("a", 1, true, 0), item("a", 1, true, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ord.Items) != 1 || ord.Duplicates() != 1 {
		t.Fatalf("items = %d duplicates = %d", len(ord.Items), ord.Duplicates())
	}
}

func TestCanonicalSequencesWrapDeterministically(t *testing.T) {
	ord, err := Order([]Item{item("a", 1, true, 0), item("b", 2, true, 1), item("c", 3, true, 2)})
	if err != nil {
		t.Fatal(err)
	}
	got := CanonicalSequences(ord, 65534)
	want := []uint16{65534, 65535, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sequence %d = %d, want %d", i, got[i], want[i])
		}
	}
	again := CanonicalSequences(ord, 65534)
	for i := range want {
		if again[i] != got[i] {
			t.Fatal("canonical sequences are not deterministic")
		}
	}
}
