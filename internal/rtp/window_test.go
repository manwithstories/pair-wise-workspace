package rtpwrap

import (
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq}, Payload: []byte{byte(seq)}}
}

func sequences(pkts []*rtp.Packet) []uint16 {
	out := make([]uint16, 0, len(pkts))
	for _, p := range pkts {
		out = append(out, p.SequenceNumber)
	}
	return out
}

func TestWindowReordersAndDedups(t *testing.T) {
	w := NewWindow(8)
	if r := w.Push(pkt(10)); r.Decision != DecisionDelivered || len(r.Ordered) != 1 {
		t.Fatalf("first packet wrong: %+v", r)
	}
	if r := w.Push(pkt(12)); r.Decision != DecisionBuffered || len(r.Ordered) != 0 {
		t.Fatalf("seq12 should buffer: %+v", r)
	}
	r := w.Push(pkt(11))
	if r.Decision != DecisionDelivered || sequences(r.Ordered)[0] != 11 || sequences(r.Ordered)[1] != 12 {
		t.Fatalf("seq11 should drain 11,12: %+v", r.Ordered)
	}
	dup := w.Push(pkt(11))
	if dup.Decision != DecisionDuplicate || w.Duplicates != 1 {
		t.Fatalf("duplicate not counted: %+v duplicates=%d", dup, w.Duplicates)
	}
}

func TestWindowOutsideOrderCountsGap(t *testing.T) {
	w := NewWindow(4)
	w.Push(pkt(10))
	w.Push(pkt(11))
	w.Push(pkt(12))
	r := w.Push(pkt(4))
	if r.Decision != DecisionOutsideWindow {
		t.Fatalf("decision = %s, want outside_window_gap", r.Decision)
	}
	if w.OutsideWindow != 1 || w.Gaps != 1 {
		t.Fatalf("outside=%d gaps=%d", w.OutsideWindow, w.Gaps)
	}
}

func TestWindowFlushSkipsMissingSequences(t *testing.T) {
	w := NewWindow(8)
	w.Push(pkt(20))
	w.Push(pkt(22))
	out := w.Flush()
	if got := sequences(out); len(got) != 1 || got[0] != 22 {
		t.Fatalf("flush = %v, want [22]", got)
	}
	if w.Gaps != 1 {
		t.Fatalf("gaps = %d, want 1", w.Gaps)
	}
}

func TestPushBytesRejectsMalformed(t *testing.T) {
	w := NewWindow(4)
	if _, err := w.PushBytes([]byte{0x00, 0x01}); err == nil {
		t.Fatal("expected malformed RTP error")
	}
}
