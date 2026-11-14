package counter

import (
	"testing"

	"tsaudit/internal/tspipe"
)

func pkt(pid uint16, cc uint8, payload string) *tspipe.Packet {
	return &tspipe.Packet{PID: pid, Continuity: cc, HasPayload: true, Payload: []byte(payload)}
}

func TestContinuityWrapAdaptationAndNull(t *testing.T) {
	tr := NewTracker()
	for _, cc := range []uint8{14, 15, 0, 1} {
		if jump := tr.Observe(pkt(0x100, cc, string(rune('a'+cc)))); jump != nil {
			t.Fatalf("unexpected jump at CC %d: %+v", cc, jump)
		}
	}
	tr.Observe(&tspipe.Packet{PID: 0x100, Continuity: 9, HasAdaptation: true})
	if jump := tr.Observe(pkt(0x100, 2, "adaptation did not advance")); jump != nil {
		t.Fatalf("adaptation-only packet advanced CC: %+v", jump)
	}
	if jump := tr.Observe(pkt(NullPID, 15, "null")); jump != nil || tr.Lost != 0 {
		t.Fatalf("null PID participated in CC tracking: %+v", jump)
	}
}

func TestDuplicateIsNotLoss(t *testing.T) {
	tr := NewTracker()
	original := pkt(0x100, 4, "same payload")
	tr.Observe(original)
	tr.Observe(pkt(0x100, 4, "same payload"))
	if tr.Duplicates != 1 || tr.Lost != 0 || len(tr.Jumps) != 0 {
		t.Fatalf("duplicate misclassified: duplicates=%d lost=%d jumps=%d", tr.Duplicates, tr.Lost, len(tr.Jumps))
	}
}

func TestJumpRecordsOffsetAndValues(t *testing.T) {
	tr := NewTracker()
	tr.Observe(pkt(0x100, 1, "first"))
	p := pkt(0x100, 5, "later")
	p.Offset = 1234
	jump := tr.Observe(p)
	if jump == nil || jump.Offset != 1234 || jump.Previous != 1 || jump.Next != 5 || jump.Lost != 3 {
		t.Fatalf("unexpected jump: %+v", jump)
	}
	if tr.Lost != 3 || len(tr.Jumps) != 1 {
		t.Fatalf("lost=%d jumps=%d", tr.Lost, len(tr.Jumps))
	}
}

func TestSameCCDifferentPayloadIsNotDuplicate(t *testing.T) {
	tr := NewTracker()
	tr.Observe(pkt(0x100, 7, "one"))
	jump := tr.Observe(pkt(0x100, 7, "two"))
	if tr.Duplicates != 0 || jump == nil || jump.Lost != 15 {
		t.Fatalf("same-CC different payload mishandled: dup=%d jump=%+v", tr.Duplicates, jump)
	}
}
