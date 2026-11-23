package rtp

import (
	"testing"

	"github.com/pion/rtp"
)

func makePkts(n int) []rtp.Packet {
	var seq uint16 = 100
	var out []rtp.Packet
	for i := 0; i < n; i++ {
		payload := []byte{byte(i), byte(i >> 8), 0x80}
		out = append(out, Packetize(payload, 42, &seq, uint32(i*3600), 1200)...)
	}
	return out
}

func drainAll(r *Recoverer, wire Wire) []rtp.Packet {
	var out []rtp.Packet
	for _, p := range wire.Packets {
		r.Push(p)
		out = append(out, r.Drain()...)
	}
	return append(out, r.Finish()...)
}

func TestPacketizeChunks(t *testing.T) {
	var seq uint16
	pkts := Packetize(make([]byte, 2500), 7, &seq, 9000, 1000)
	if len(pkts) != 3 {
		t.Fatalf("pkts=%d, want 3", len(pkts))
	}
	if !pkts[2].Marker || pkts[0].Marker {
		t.Fatal("marker must be set only on last packet")
	}
	if seq != 3 {
		t.Fatalf("seq=%d", seq)
	}
}

func TestRecoverBurstLoss(t *testing.T) {
	pkts := makePkts(40)
	wire := Simulate(pkts, 10, 8, 0, 0)
	if len(wire.Lost) != 8 {
		t.Fatalf("lost=%d", len(wire.Lost))
	}
	r := NewRecoverer(pkts, 3, 13.3, 50)
	out := drainAll(r, wire)
	if len(out) != 40 {
		t.Fatalf("released=%d, want 40", len(out))
	}
	if r.Stats.Lost != 8 || r.Stats.Recovered != 8 {
		t.Fatalf("stats=%+v", r.Stats)
	}
	if got := r.Stats.RecoveryRate(); got != 1.0 {
		t.Fatalf("rate=%v", got)
	}
	for i := 1; i < len(out); i++ {
		if out[i].SequenceNumber != out[i-1].SequenceNumber+1 {
			t.Fatal("output not in sequence")
		}
	}
}

func TestRecoverDupAndReorder(t *testing.T) {
	pkts := makePkts(30)
	wire := Simulate(pkts, -1, 0, 5, 7)
	r := NewRecoverer(pkts, 3, 13.3, 50)
	out := drainAll(r, wire)
	if len(out) != 30 {
		t.Fatalf("released=%d, want 30", len(out))
	}
	if r.Stats.Duplicates == 0 || r.Stats.Reordered == 0 {
		t.Fatalf("stats=%+v", r.Stats)
	}
	if r.Stats.Lost != 0 {
		t.Fatalf("false losses: %+v", r.Stats)
	}
}

func TestUnrecoverableGap(t *testing.T) {
	pkts := makePkts(20)
	wire := Simulate(pkts, 5, 3, 0, 0)
	// Sender buffer no longer holds the lost packets: unrecoverable.
	src := append([]rtp.Packet{}, pkts[:5]...)
	src = append(src, pkts[8:]...)
	r := NewRecoverer(src, 3, 13.3, 50)
	out := drainAll(r, wire)
	if len(out) != 17 {
		t.Fatalf("released=%d, want 17", len(out))
	}
	if r.Stats.Unrecovered() != 3 {
		t.Fatalf("stats=%+v", r.Stats)
	}
	if got := r.Stats.RecoveryRate(); got != 0 {
		t.Fatalf("rate=%v", got)
	}
}
