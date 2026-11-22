package fault

import (
	"errors"
	"strings"
	"testing"
)

func TestFaultValidationAndOrdering(t *testing.T) {
	if _, err := New("nope", Location{}, "", true, true); err == nil {
		t.Fatal("expected unknown kind error")
	}
	if _, err := New(KindBoxOverflow, Location{Offset: -2}, "", true, true); err == nil {
		t.Fatal("expected invalid offset error")
	}
	var l List
	l.Add(
		MustNew(KindMissingMdat, Location{Segment: "b.m4s", Offset: 12}, "no mdat", true, false),
		MustNew(KindTruncatedTail, Location{Segment: "a.m4s", Offset: 40, Size: 100}, "tail", true, true),
		MustNew(KindBoxOverflow, Location{Segment: "a.m4s", Offset: 8}, "size", true, false),
	)
	got := l.All()
	if len(got) != 3 || got[0].Loc.Segment != "a.m4s" || got[0].Kind != KindBoxOverflow {
		t.Fatalf("unexpected order: %+v", got)
	}
	if got[1].Kind != KindTruncatedTail {
		t.Fatalf("offset sort failed: %+v", got)
	}
	if n := l.ByKind()[KindBoxOverflow]; n != 1 {
		t.Fatalf("ByKind = %d", n)
	}
	if l.Recoverable() != true {
		t.Fatal("all faults are recoverable")
	}
	if segs := l.SegmentsWithFaults(); len(segs) != 2 || segs[0] != "a.m4s" {
		t.Fatalf("segments = %v", segs)
	}
	err := l.Err()
	var single Fault
	if !errors.As(err, &single) || single.Kind != KindBoxOverflow {
		t.Fatalf("joined error did not unwrap to a Fault: %v", err)
	}
	if !strings.Contains(err.Error(), "a.m4s") || !strings.Contains(err.Error(), "offset=8") {
		t.Fatalf("error lacks location: %v", err)
	}
}

func TestEmptyList(t *testing.T) {
	var l List
	if l.Err() != nil || !l.Recoverable() || l.Len() != 0 {
		t.Fatal("zero-value List must be usable")
	}
}
