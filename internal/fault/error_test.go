package fault

import "testing"

func TestKindRecoverability(t *testing.T) {
	if !KindContinuity.Recoverable() || !KindTruncated.Recoverable() {
		t.Fatal("continuity and truncation are recoverable gaps")
	}
	if KindPSI.Recoverable() || KindMetadata.Recoverable() {
		t.Fatal("PSI loss and metadata mismatch are not recoverable by keyframe skip")
	}
	if SeverityOf(KindCRC) != SevFatal || SeverityOf(KindTruncated) != SevRepairable {
		t.Fatal("severity mapping")
	}
	if SeverityOf(KindMetadata) != SevInfo {
		t.Fatal("metadata differences are informational")
	}
}

func TestListOrderingAndFirst(t *testing.T) {
	var l List
	l.Add(Error{Kind: KindContinuity, Offset: 752, Detail: "cc jump"})
	l.Add(Error{Kind: KindCRC, Offset: 940, PID: 0})
	l.Add(Error{Kind: KindTruncated, Offset: 1000})
	l.Add(Error{Kind: KindContinuity, Offset: 752, PID: 256})

	sorted := l.Sorted()
	if sorted[0].Kind != KindContinuity || sorted[0].Offset != 752 {
		t.Fatalf("unexpected head: %+v", sorted[0])
	}
	if sorted[1].PID != 256 {
		t.Fatalf("expected stable sort tiebreak by PID, got %+v", sorted[1])
	}
	if got := l.First(); got != 752 {
		t.Fatalf("first=%d", got)
	}
	if _, ok := l.FirstFatal(); !ok {
		t.Fatal("CRC should be fatal")
	}
	if got, _ := l.FirstFatal(); got != 940 {
		t.Fatalf("first fatal=%d", got)
	}
	// original order preserved
	if l.Items()[0].Offset != 752 || l.Items()[1].Offset != 940 {
		t.Fatal("Sorted mutated the list")
	}
}

func TestErrorStringStable(t *testing.T) {
	e := Error{Kind: KindContinuity, PID: 0x100, Offset: 188, Packet: 1, Detail: "expected 5 got 7"}
	const want = "continuity pid=0x0100 off=188 pkt=1 expected 5 got 7"
	if got := e.String(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	var empty List
	if empty.First() != -1 {
		t.Fatal("empty first should be -1")
	}
	if _, ok := empty.FirstFatal(); ok {
		t.Fatal("empty list has no fatal")
	}
}
