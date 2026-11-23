package repair

import (
	"testing"

	"archivecheck/internal/fault"
	"archivecheck/internal/parse"
)

func frag(seg string, seq uint32, has bool) *parse.Fragment {
	return &parse.Fragment{Segment: seg, Sequence: seq, HasMfhd: has}
}

func TestOrderRepairsGapAndOrder(t *testing.T) {
	frags := []*parse.Fragment{
		frag("f3", 3, true),
		frag("f1", 1, true),
		frag("f2", 2, true),
		frag("f5", 5, true),
		frag("f6", 6, true),
	}
	ord, err := Order(frags)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range ord.Fragments {
		names = append(names, f.Segment)
	}
	want := "f1,f2,f3,f5,f6"
	got := ""
	for i, n := range names {
		if i > 0 {
			got += ","
		}
		got += n
	}
	if got != want {
		t.Fatalf("order = %s, want %s", got, want)
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
	ord, err := Order([]*parse.Fragment{frag("a", 1, true), frag("b", 0, false), frag("c", 2, true)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ord.Fragments) != 3 {
		t.Fatalf("fragments = %d", len(ord.Fragments))
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
	_, err := Order([]*parse.Fragment{frag("a", 1, true), frag("b", 1, true)})
	if err == nil || !fault.IsKind(err, fault.KindSequence) {
		t.Fatalf("err = %v", err)
	}
}

func TestOrderDropsIdenticalDuplicate(t *testing.T) {
	f := frag("a", 1, true)
	ord, err := Order([]*parse.Fragment{f, f})
	if err != nil {
		t.Fatal(err)
	}
	if len(ord.Fragments) != 1 || ord.Duplicates() != 1 {
		t.Fatalf("fragments = %d duplicates = %d", len(ord.Fragments), ord.Duplicates())
	}
}
