package rtp

import (
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 1},
		Payload: []byte{byte(seq)},
	}
}

func seqs(pkts []*rtp.Packet) []uint16 {
	out := make([]uint16, len(pkts))
	for i, p := range pkts {
		out[i] = p.SequenceNumber
	}
	return out
}

func TestInOrderPassthrough(t *testing.T) {
	r := NewRepairer(8, 90000)
	for i := 0; i < 20; i++ {
		r.Push(pkt(uint16(i), uint32(i*3600)))
	}
	r.Flush()
	got := seqs(r.Take())
	if len(got) != 20 {
		t.Fatalf("emitted %d packets", len(got))
	}
	for i, s := range got {
		if s != uint16(i) {
			t.Fatalf("out of order at %d: %v", i, got)
		}
	}
	if st := r.Stats(); st.Reordered != 0 || st.Lost != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestSequenceWrapAround(t *testing.T) {
	r := NewRepairer(8, 90000)
	seq := []uint16{65533, 65534, 65535, 0, 1, 2}
	for i, s := range seq {
		r.Push(pkt(s, uint32((i+65533)*3600)))
	}
	r.Flush()
	got := seqs(r.Take())
	if len(got) != len(seq) {
		t.Fatalf("emitted %v", got)
	}
	for i, s := range seq {
		if got[i] != s {
			t.Fatalf("wrap order broken: got %v want %v", got, seq)
		}
	}
}

func TestDuplicateDropped(t *testing.T) {
	r := NewRepairer(8, 90000)
	r.Push(pkt(10, 1000))
	r.Push(pkt(12, 1200)) // pending: 11 missing
	r.Push(pkt(12, 1200)) // duplicate of a pending packet
	r.Push(pkt(11, 1100))
	r.Flush()
	if got := r.Take(); len(got) != 3 {
		t.Fatalf("emitted %d packets", len(got))
	}
	if st := r.Stats(); st.Duplicate != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestSingleLossRecoveredByRetransmit(t *testing.T) {
	r := NewRepairer(8, 90000)
	order := []uint16{0, 1, 2, 3, 5, 6, 7, 4, 8, 9} // 4 retransmitted late
	for i, s := range order {
		r.Push(pkt(s, uint32(s*3600)))
		_ = i
	}
	r.Flush()
	got := seqs(r.Take())
	for i, s := range got {
		if s != uint16(i) {
			t.Fatalf("recovered order broken: %v", got)
		}
	}
	st := r.Stats()
	if st.Recovered != 1 || st.Lost != 0 || st.RecoveryPct() != 100 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestBurstLossReportedAsGap(t *testing.T) {
	r := NewRepairer(4, 90000)
	for s := uint16(0); s < 10; s++ {
		r.Push(pkt(s, uint32(s)*3600))
	}
	// 10..15 lost in a burst (never arrive)
	for s := uint16(16); s < 26; s++ {
		r.Push(pkt(s, uint32(s)*3600))
	}
	r.Flush()
	st := r.Stats()
	if st.Lost != 6 {
		t.Fatalf("lost = %d, want 6 (%+v)", st.Lost, st)
	}
	gaps := r.Gaps()
	if len(gaps) != 1 {
		t.Fatalf("gaps = %v", gaps)
	}
	g := gaps[0]
	if g.StartSeq != 10 || g.EndSeq != 16 {
		t.Fatalf("gap seqs = [%d,%d)", g.StartSeq, g.EndSeq)
	}
	if g.Start.Millis() != 360 || g.End.Millis() != 640 {
		t.Fatalf("gap times = [%v,%v) ms", g.Start.Millis(), g.End.Millis())
	}
}

func TestLatePacketCounted(t *testing.T) {
	r := NewRepairer(2, 90000)
	r.Push(pkt(0, 0))
	r.Push(pkt(3, 10800)) // declares 1,2 lost once window exceeded
	r.Push(pkt(4, 14400))
	r.Push(pkt(1, 3600)) // too late
	r.Flush()
	if st := r.Stats(); st.Late != 1 {
		t.Fatalf("stats = %+v", st)
	}
	got := seqs(r.Take())
	if len(got) != 3 || got[1] != 3 {
		t.Fatalf("emitted %v", got)
	}
}

func TestDuplicateAfterEmissionCounted(t *testing.T) {
	r := NewRepairer(8, 90000)
	for s := uint16(0); s < 5; s++ {
		r.Push(pkt(s, uint32(s)*3600))
	}
	r.Take() // emits 0..4; cursor now at 5
	r.Push(pkt(2, 7200))
	r.Flush()
	st := r.Stats()
	if st.Duplicate != 1 {
		t.Fatalf("Duplicate=%d, want 1 (%+v)", st.Duplicate, st)
	}
	if st.Late != 0 {
		t.Fatalf("Late=%d, want 0 for an emitted duplicate (%+v)", st.Late, st)
	}
}
