package repair

import (
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16, payload ...byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, SSRC: 1},
		Payload: payload,
	}
}

func feed(r *Reassembler, pkts ...*rtp.Packet) ([]Emitted, []Event) {
	var em []Emitted
	var ev []Event
	for i, p := range pkts {
		e, v := r.Push(1, p, i)
		em = append(em, e...)
		ev = append(ev, v...)
	}
	return em, ev
}

func seqs(em []Emitted) []uint64 {
	out := make([]uint64, len(em))
	for i, e := range em {
		out[i] = e.ExtSeq
	}
	return out
}

func TestInOrder(t *testing.T) {
	r := NewReassembler(16)
	em, ev := feed(r, pkt(0, 1), pkt(1, 2), pkt(2, 3))
	if len(ev) != 0 || len(em) != 3 {
		t.Fatalf("em=%v ev=%v", seqs(em), ev)
	}
}

func TestReorderWithinWindow(t *testing.T) {
	r := NewReassembler(16)
	em, ev := feed(r, pkt(0), pkt(2), pkt(1), pkt(3))
	if got := seqs(em); len(got) != 4 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("order=%v", got)
	}
	if len(ev) != 1 || ev[0].Kind != EvReorder || ev[0].ExtSeq != 1 {
		t.Fatalf("ev=%v", ev)
	}
}

func TestLossDeclaredAfterWindow(t *testing.T) {
	r := NewReassembler(8)
	var pkts []*rtp.Packet
	for i := 0; i <= 12; i++ {
		if i == 3 { // seq 3 never arrives
			continue
		}
		pkts = append(pkts, pkt(uint16(i)))
	}
	em, ev := feed(r, pkts...)
	var loss *Event
	for i := range ev {
		if ev[i].Kind == EvLoss {
			loss = &ev[i]
		}
	}
	if loss == nil || loss.ExtSeq != 3 || loss.Count != 1 {
		t.Fatalf("loss=%+v ev=%v", loss, ev)
	}
	if got := seqs(em); len(got) != 12 || got[3] != 4 {
		t.Fatalf("em=%v", got)
	}
}

func TestExactDuplicateDropped(t *testing.T) {
	r := NewReassembler(16)
	em, ev := feed(r, pkt(0, 9), pkt(1, 8), pkt(1, 8))
	if len(em) != 2 {
		t.Fatalf("em=%v", seqs(em))
	}
	if len(ev) != 1 || ev[0].Kind != EvDuplicate {
		t.Fatalf("ev=%v", ev)
	}
}

func TestConflictNeverOverwrites(t *testing.T) {
	r := NewReassembler(16)
	em, ev := feed(r, pkt(0, 0xAA), pkt(0, 0xBB))
	if len(em) != 1 || em[0].Packet.Payload[0] != 0xAA {
		t.Fatalf("first copy must win: em=%v", em)
	}
	if len(ev) != 1 || ev[0].Kind != EvConflict {
		t.Fatalf("ev=%v", ev)
	}
}

func TestLateAfterWindow(t *testing.T) {
	r := NewReassembler(8)
	var pkts []*rtp.Packet
	for i := 0; i <= 12; i++ {
		if i == 3 {
			continue
		}
		pkts = append(pkts, pkt(uint16(i)))
	}
	pkts = append(pkts, pkt(3)) // arrives too late
	_, ev := feed(r, pkts...)
	var late bool
	for _, e := range ev {
		if e.Kind == EvLate && e.ExtSeq == 3 {
			late = true
		}
	}
	if !late {
		t.Fatalf("ev=%v", ev)
	}
}

func TestSequenceWraparound(t *testing.T) {
	r := NewReassembler(16)
	em, ev := feed(r, pkt(65534), pkt(65535), pkt(0), pkt(1))
	if len(ev) != 0 {
		t.Fatalf("ev=%v", ev)
	}
	got := seqs(em)
	want := []uint64{65534, 65535, 65536, 65537}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got=%v want=%v", got, want)
		}
	}
}

func TestFlushDeclaresTailGap(t *testing.T) {
	r := NewReassembler(64)
	feed(r, pkt(0), pkt(1), pkt(5))
	em, ev := r.Flush(1)
	var loss *Event
	for i := range ev {
		if ev[i].Kind == EvLoss {
			loss = &ev[i]
		}
	}
	if loss == nil || loss.ExtSeq != 2 || loss.Count != 3 {
		t.Fatalf("loss=%+v", loss)
	}
	if got := seqs(em); len(got) != 1 || got[0] != 5 {
		t.Fatalf("em=%v", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	r := NewReassembler(16)
	feed(r, pkt(0, 1), pkt(2, 3)) // seq 1 buffered gap
	st := r.State()
	r2 := NewReassembler(16)
	if err := r2.Restore(st); err != nil {
		t.Fatal(err)
	}
	em, _ := feed(r2, pkt(1, 2))
	got := seqs(em)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("after restore: %v", got)
	}
}
