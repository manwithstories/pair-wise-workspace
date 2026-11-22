package model

import (
	"errors"
	"testing"
)

func TestClassifyRoundTrip(t *testing.T) {
	err := Classify(KindCorrupt, errors.New("bad payload"))
	if KindOf(err) != KindCorrupt {
		t.Fatalf("kind = %q", KindOf(err))
	}
	// Re-classification keeps the first kind.
	if got := KindOf(Classify(KindLate, err)); got != KindCorrupt {
		t.Fatalf("reclassified kind = %q", got)
	}
	if KindOf(errors.New("plain")) != KindNone {
		t.Fatal("plain error should have no kind")
	}
	if got := Classify(KindOversize, nil); KindOf(got) != KindOversize {
		t.Fatalf("nil error kind = %q", KindOf(got))
	}
}

func TestPayloadHashEmptyAndSame(t *testing.T) {
	a := Event{Payload: []byte("abc")}
	b := Event{Payload: []byte("abc")}
	if a.PayloadHash() != b.PayloadHash() {
		t.Fatal("equal payloads must hash equally")
	}
	if (Event{}).PayloadHash() == a.PayloadHash() {
		t.Fatal("empty and non-empty payloads collide")
	}
}

func TestTimeRange(t *testing.T) {
	r := TimeRange{Start: 10, End: 20}
	if !r.Contains(10) || r.Contains(20) || !r.Contains(19) {
		t.Fatal("half-open containment broken")
	}
	if !r.Overlaps(TimeRange{Start: 19, End: 30}) || r.Overlaps(TimeRange{Start: 20, End: 30}) {
		t.Fatal("overlap broken")
	}
}

func TestAllKindsUnique(t *testing.T) {
	seen := map[ErrorKind]bool{}
	for _, k := range AllErrorKinds {
		if seen[k] {
			t.Fatalf("duplicate kind %q", k)
		}
		seen[k] = true
	}
}
