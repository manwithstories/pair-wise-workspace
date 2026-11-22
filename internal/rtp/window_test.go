package rtp

import (
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 3600}}
}

func seqs(pkts []*rtp.Packet) []uint16 {
	out := make([]uint16, len(pkts))
	for i, p := range pkts {
		out[i] = p.SequenceNumber
	}
	return out
}

func TestInOrderDelivery(t *testing.T) {
	w, _ := NewWindow(8)
	var got []uint16
	for _, s := range []uint16{10, 11, 12} {
		out, err := w.Push(pkt(s))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, seqs(out)...)
	}
	if len(got) != 3 || got[0] != 10 || got[2] != 12 {
		t.Fatalf("in-order delivery broken: %v", got)
	}
}

func TestDuplicateCountedOnce(t *testing.T) {
	w, _ := NewWindow(8)
	_, _ = w.Push(pkt(1))
	_, _ = w.Push(pkt(2))
	dup, err := w.Push(pkt(1)) // duplicate
	if err != nil || len(dup) != 0 {
		t.Fatalf("duplicate must be swallowed, got %v err=%v", dup, err)
	}
	m := w.Metrics()
	if m.Duplicates != 1 || m.Received != 2 {
		t.Fatalf("dup=%d received=%d", m.Duplicates, m.Received)
	}
}

func TestReorderFillsGapAndRecovers(t *testing.T) {
	w, _ := NewWindow(8)
	_, _ = w.Push(pkt(0))
	hold, _ := w.Push(pkt(2)) // gap at 1, nothing deliverable
	if len(hold) != 0 {
		t.Fatal("gap packet should not deliver before hole fills")
	}
	got, err := w.Push(pkt(1)) // late fill, drains 1 then 2
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SequenceNumber != 1 || got[1].SequenceNumber != 2 {
		t.Fatalf("reordered delivery wrong: %v", seqs(got))
	}
	m := w.Metrics()
	if m.GapPackets != 1 || m.Recovered != 1 || m.RecoveryRate() != 1.0 {
		t.Fatalf("gap metrics wrong: %+v", m)
	}
}

func TestOverflowRecordsGapWithoutFabrication(t *testing.T) {
	w, _ := NewWindow(4)
	_, _ = w.Push(pkt(0))
	// Jump beyond capacity: must record a gap, never invent packets.
	got, err := w.Push(pkt(20))
	if err != nil {
		t.Fatalf("overflow should be handled, not error: %v", err)
	}
	if len(got) != 1 || got[0].SequenceNumber != 20 {
		t.Fatalf("window should re-anchor at seq 20, got %v", seqs(got))
	}
	m := w.Metrics()
	if m.GapPackets < 1 {
		t.Fatalf("overflow gap not recorded: %+v", m)
	}
}

func TestLatePacketAfterFlushRejected(t *testing.T) {
	w, _ := NewWindow(8)
	_, _ = w.Push(pkt(0))
	_, _ = w.Push(pkt(1))
	w.Flush()
	late, err := w.Push(pkt(0))
	if err != nil || late != nil {
		t.Fatalf("post-flush late packet must be dropped, got %v err=%v", late, err)
	}
	if w.Metrics().LateDrop != 1 {
		t.Fatalf("late drop not counted: %+v", w.Metrics())
	}
}

func TestUnrecoveredHoleLowersRecoveryRate(t *testing.T) {
	w, _ := NewWindow(16)
	_, _ = w.Push(pkt(0))
	_, _ = w.Push(pkt(5)) // opens a 4-sequence hole
	w.Flush()             // hole never filled
	m := w.Metrics()
	if m.GapPackets != 4 || m.Recovered != 0 {
		t.Fatalf("want 4 unrecovered, got %+v", m)
	}
	if m.RecoveryRate() != 0 {
		t.Fatalf("recovery rate must be 0, got %f", m.RecoveryRate())
	}
}
