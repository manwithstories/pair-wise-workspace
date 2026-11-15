package tsdemux

import "testing"

func ccPkt(pid uint16, cc uint8, discont bool) *Packet {
	return &Packet{PID: pid, CC: cc, HasPayload: true, Discontinuity: discont}
}

func TestCCNormalAndWrap(t *testing.T) {
	tr := NewCCTracker()
	for cc := uint8(0); cc < 20; cc++ { // includes 15 -> 0 wrap
		tr.Observe(ccPkt(100, cc%16, false))
	}
	j, l, d := tr.Stats(100)
	if j != 0 || l != 0 || d != 0 {
		t.Fatalf("got jumps=%d lost=%d dup=%d", j, l, d)
	}
}

func TestCCDuplicate(t *testing.T) {
	tr := NewCCTracker()
	tr.Observe(ccPkt(1, 5, false))
	tr.Observe(ccPkt(1, 5, false)) // duplicate
	tr.Observe(ccPkt(1, 6, false))
	j, l, d := tr.Stats(1)
	if j != 0 || l != 0 || d != 1 {
		t.Fatalf("got jumps=%d lost=%d dup=%d", j, l, d)
	}
}

func TestCCJumpEstimatesLost(t *testing.T) {
	tr := NewCCTracker()
	tr.Observe(ccPkt(1, 3, false))
	tr.Observe(ccPkt(1, 7, false)) // 4,5,6 missing
	j, l, _ := tr.Stats(1)
	if j != 1 || l != 3 {
		t.Fatalf("got jumps=%d lost=%d", j, l)
	}
	tr.Observe(ccPkt(1, 8, false)) // back to normal
	j, l, _ = tr.Stats(1)
	if j != 1 || l != 3 {
		t.Fatalf("got jumps=%d lost=%d", j, l)
	}
}

func TestCCDiscontinuityIndicator(t *testing.T) {
	tr := NewCCTracker()
	tr.Observe(ccPkt(1, 3, false))
	tr.Observe(ccPkt(1, 9, true)) // discontinuity: not a loss
	tr.Observe(ccPkt(1, 10, false))
	j, l, d := tr.Stats(1)
	if j != 0 || l != 0 || d != 0 {
		t.Fatalf("got jumps=%d lost=%d dup=%d", j, l, d)
	}
}

func TestCCSkipsNullTEIAndAdaptationOnly(t *testing.T) {
	tr := NewCCTracker()
	tr.Observe(ccPkt(1, 3, false))
	tr.Observe(&Packet{PID: NullPID, CC: 0, HasPayload: true})
	tr.Observe(&Packet{PID: 1, CC: 9, TEI: true, HasPayload: true})
	tr.Observe(&Packet{PID: 1, CC: 9, HasPayload: false}) // adaptation only
	tr.Observe(ccPkt(1, 4, false))
	j, l, d := tr.Stats(1)
	if j != 0 || l != 0 || d != 0 {
		t.Fatalf("got jumps=%d lost=%d dup=%d", j, l, d)
	}
}
