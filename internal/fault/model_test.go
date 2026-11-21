package fault

import (
	"sort"
	"testing"
)

func TestListOrderingAndCount(t *testing.T) {
	l := List{
		{Category: CatCCGap, PacketIndex: 9, Recoverable: true},
		{Category: CatTEI, PacketIndex: 3, Recoverable: true},
		{Category: CatCCGap, PacketIndex: 3, Recoverable: true},
	}
	sort.Sort(l)
	if l[0].Category != CatCCGap || l[1].Category != CatTEI || l[2].Category != CatCCGap {
		t.Fatalf("unexpected order: %v", l)
	}
	if got := l.Count(CatCCGap); got != 2 {
		t.Fatalf("Count(CCGap)=%d want 2", got)
	}
	if l.HasUnrecoverable() {
		t.Fatal("all faults recoverable, HasUnrecoverable must be false")
	}
}

func TestHasUnrecoverable(t *testing.T) {
	l := List{{Category: CatSyncLoss, PacketIndex: 1, Recoverable: false}}
	if !l.HasUnrecoverable() {
		t.Fatal("expected unrecoverable detection")
	}
}

func TestString(t *testing.T) {
	f := Fault{Category: CatCCGap, PID: 0x101, PacketIndex: 42, MediaTime: UnknownTime, Recoverable: true, Detail: "gap=3"}
	s := f.String()
	if s == "" || s[0] != 'c' {
		t.Fatalf("unexpected string %q", s)
	}
}
