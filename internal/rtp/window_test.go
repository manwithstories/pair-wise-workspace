package rtpwin

import (
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 3000}}
}

func seqs(pkts []*rtp.Packet) []uint16 {
	var out []uint16
	for _, p := range pkts {
		out = append(out, p.SequenceNumber)
	}
	return out
}

func TestInOrder(t *testing.T) {
	w := New(8, 16)
	var got []uint16
	for s := uint16(0); s < 5; s++ {
		got = append(got, seqs(w.Push(pkt(s)))...)
	}
	got = append(got, seqs(w.Finish())...)
	if len(got) != 5 || w.Stats.Lost != 0 || w.Stats.Recovered != 0 {
		t.Fatalf("got=%v stats=%+v", got, w.Stats)
	}
}

func TestReorderAndDuplicate(t *testing.T) {
	w := New(8, 16)
	var got []uint16
	got = append(got, seqs(w.Push(pkt(0)))...)
	got = append(got, seqs(w.Push(pkt(2)))...) // gap at 1
	got = append(got, seqs(w.Push(pkt(1)))...) // fills gap
	got = append(got, seqs(w.Push(pkt(1)))...) // duplicate
	got = append(got, seqs(w.Finish())...)
	if len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("got=%v", got)
	}
	if w.Stats.Duplicates != 1 {
		t.Fatalf("stats=%+v", w.Stats)
	}
}

func TestRecoveryFromCache(t *testing.T) {
	w := New(8, 16)
	var got []uint16
	got = append(got, seqs(w.Push(pkt(0)))...)
	w.CacheAdd(pkt(1)) // sender keeps a retransmission copy
	got = append(got, seqs(w.Push(pkt(2)))...) // gap at 1 -> cache hit
	got = append(got, seqs(w.Finish())...)
	if len(got) != 3 || w.Stats.Recovered != 1 || w.Stats.Lost != 0 {
		t.Fatalf("got=%v stats=%+v", got, w.Stats)
	}
	if w.Stats.RecoveryPct() != 100 {
		t.Fatalf("pct=%v", w.Stats.RecoveryPct())
	}
}

func TestBurstLossDeclared(t *testing.T) {
	w := New(4, 0) // no cache
	var got []uint16
	got = append(got, seqs(w.Push(pkt(0)))...)
	// Burst loss of 1..4, window 4 forces loss declarations.
	got = append(got, seqs(w.Push(pkt(5)))...)
	got = append(got, seqs(w.Finish())...)
	if w.Stats.Lost != 4 {
		t.Fatalf("stats=%+v", w.Stats)
	}
	if got[len(got)-1] != 5 {
		t.Fatalf("got=%v", got)
	}
}

func TestSequenceWrap(t *testing.T) {
	w := New(8, 16)
	var got []uint16
	for _, s := range []uint16{65534, 65535, 0, 1} {
		got = append(got, seqs(w.Push(pkt(s)))...)
	}
	got = append(got, seqs(w.Finish())...)
	if len(got) != 4 || got[3] != 1 {
		t.Fatalf("wrap got=%v", got)
	}
}
