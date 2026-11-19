package segment

import "testing"

func TestOrderDedupSortGap(t *testing.T) {
	segs := []Segment{
		New(2, "s2.m4s", []byte("two")),
		New(0, "s0.m4s", []byte("zero")),
		New(2, "s2.copy.m4s", []byte("two")), // identical duplicate
		New(1, "s1.m4s", []byte("one")),
	}
	r, err := Order("a", segs, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Ordered) != 3 || r.Duplicates != 1 {
		t.Fatalf("got %+v", r)
	}
	for i, s := range r.Ordered {
		if s.Index != i {
			t.Fatalf("out of order at %d: %+v", i, s)
		}
	}
	if len(r.Gaps) != 1 || r.Gaps[0] != 3 {
		t.Fatalf("gaps %v", r.Gaps)
	}
}

func TestOrderConflictKeepsFirst(t *testing.T) {
	segs := []Segment{
		New(3, "s3.m4s", []byte("original")),
		New(3, "s3.stale.m4s", []byte("tampered")),
	}
	r, err := Order("a", segs, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) != 1 {
		t.Fatalf("expected conflict, got %+v", r)
	}
	if string(r.Ordered[0].Data) != "original" {
		t.Fatalf("first-seen content not kept: %q", r.Ordered[0].Data)
	}
	if r.Conflicts[0].DroppedURI != "s3.stale.m4s" {
		t.Fatalf("conflict %+v", r.Conflicts[0])
	}
}

func TestOrderLimits(t *testing.T) {
	segs := make([]Segment, MaxSegments+1)
	if _, err := Order("a", segs, 0, -1); err == nil {
		t.Fatal("segment limit not enforced")
	}
}
