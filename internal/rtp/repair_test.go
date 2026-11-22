package rtp

import (
	"testing"
)

func mkPackets(track string, seqs []uint16, redundant bool) []*Packet {
	payloadOf := func(s uint16) []byte { return []byte{byte(s), byte(s >> 8), 0xAB} }
	var out []*Packet
	for _, s := range seqs {
		p := &Packet{Track: track, Seq: s, Timestamp: uint32(s) * 3000, Payload: payloadOf(s)}
		if redundant && s > 0 {
			// The sender attaches the previous sequence's payload.
			p.Redundant = payloadOf(s - 1)
		}
		out = append(out, p)
	}
	return out
}

func drainAll(b *Buffer) *Release {
	rel := b.Drain()
	tail := b.Flush()
	rel.Packets = append(rel.Packets, tail.Packets...)
	rel.Gaps = append(rel.Gaps, tail.Gaps...)
	return rel
}

func TestReorderWithinWindow(t *testing.T) {
	b := NewBuffer(8)
	pkts := mkPackets("video", []uint16{0, 1, 2, 3, 4}, false)
	// Arrive reordered: 0, 2, 1, 4, 3.
	for _, i := range []int{0, 2, 1, 4, 3} {
		if !b.Push(pkts[i]) {
			t.Fatalf("packet %d rejected", i)
		}
	}
	rel := drainAll(b)
	if len(rel.Packets) != 5 {
		t.Fatalf("released %d packets, want 5", len(rel.Packets))
	}
	for i, p := range rel.Packets {
		if p.ExtSeq != int64(i) {
			t.Fatalf("packet %d has ext seq %d", i, p.ExtSeq)
		}
	}
	if len(rel.Gaps) != 0 {
		t.Fatalf("unexpected gaps %v", rel.Gaps)
	}
}

func TestDuplicatesCounted(t *testing.T) {
	b := NewBuffer(8)
	pkts := mkPackets("audio", []uint16{10, 11, 12}, false)
	for _, p := range pkts {
		b.Push(p)
	}
	if b.Push(pkts[1]) {
		t.Fatal("duplicate accepted")
	}
	if b.Duplicates() != 1 {
		t.Fatalf("duplicates = %d, want 1", b.Duplicates())
	}
}

func TestLatePacketRejectedAfterRelease(t *testing.T) {
	b := NewBuffer(2)
	pkts := mkPackets("video", []uint16{0, 1, 2, 3, 4, 5}, false)
	for _, p := range pkts[:4] {
		b.Push(p)
	}
	rel := drainAll(b)
	if len(rel.Packets) == 0 {
		t.Fatal("nothing released")
	}
	// Packet 0 is behind the released edge now.
	if b.Push(pkts[0]) {
		t.Fatal("late packet rewrote released region")
	}
	if b.Late() != 1 {
		t.Fatalf("late = %d, want 1", b.Late())
	}
	_ = pkts
}

func TestSequenceWraparound(t *testing.T) {
	b := NewBuffer(8)
	seqs := []uint16{65534, 65535, 0, 1, 2}
	pkts := mkPackets("video", seqs, false)
	for _, p := range pkts {
		b.Push(p)
	}
	rel := drainAll(b)
	if len(rel.Packets) != 5 {
		t.Fatalf("released %d, want 5", len(rel.Packets))
	}
	for i := 1; i < len(rel.Packets); i++ {
		if rel.Packets[i].ExtSeq != rel.Packets[i-1].ExtSeq+1 {
			t.Fatalf("ext seq not contiguous at %d: %d -> %d",
				i, rel.Packets[i-1].ExtSeq, rel.Packets[i].ExtSeq)
		}
	}
	if rel.Packets[2].Seq != 0 || rel.Packets[2].ExtSeq != 65536 {
		t.Fatalf("wrapped seq 0 mapped to ext %d", rel.Packets[2].ExtSeq)
	}
}

func TestIsolatedLossRecoveredFromRedundancy(t *testing.T) {
	b := NewBuffer(4)
	// Drop seq 2; every packet carries the previous payload.
	seqs := []uint16{0, 1, 3, 4, 5}
	for _, p := range mkPackets("audio", seqs, true) {
		b.Push(p)
	}
	rel := drainAll(b)
	res := Repair(rel)
	if res.Recoverable != 1 || res.Recovered != 1 {
		t.Fatalf("recoverable=%d recovered=%d, want 1/1", res.Recoverable, res.Recovered)
	}
	if len(res.Unrecoverable) != 0 {
		t.Fatalf("unexpected unrecoverable %v", res.Unrecoverable)
	}
	if len(res.LostRaw) != 1 || res.LostRaw[0][0] != 2 {
		t.Fatalf("lost raw ranges = %v, want [[2 2]]", res.LostRaw)
	}
	// The recovered packet sits in position with payload of seq 2.
	var rec *Packet
	for _, p := range res.Packets {
		if p.Recovered {
			rec = p
		}
	}
	if rec == nil || rec.ExtSeq != 2 || rec.Payload[0] != 2 {
		t.Fatalf("recovered packet = %+v", rec)
	}
}

func TestBurstLossUnrecoverable(t *testing.T) {
	b := NewBuffer(4)
	// Burst loss of 3: seqs 2,3,4 missing.
	seqs := []uint16{0, 1, 5, 6, 7}
	for _, p := range mkPackets("audio", seqs, true) {
		b.Push(p)
	}
	rel := drainAll(b)
	res := Repair(rel)
	if res.Recovered != 0 {
		t.Fatalf("burst partially recovered: %d", res.Recovered)
	}
	if len(res.Unrecoverable) != 1 || res.Unrecoverable[0] != [2]int64{2, 4} {
		t.Fatalf("unrecoverable = %v, want [[2 4]]", res.Unrecoverable)
	}
}

func TestParseRoundTrip(t *testing.T) {
	wire, err := Marshal(7, 21000, 99, []byte("main"), []byte("prev"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(wire, "video", 1234)
	if err != nil {
		t.Fatal(err)
	}
	if p.Seq != 7 || p.Timestamp != 21000 || p.Arrival != 1234 {
		t.Fatalf("parsed %+v", p)
	}
	if string(p.Payload) != "main" || string(p.Redundant) != "prev" {
		t.Fatalf("payload %q redundant %q", p.Payload, p.Redundant)
	}
	if _, err := Parse([]byte{0, 1}, "video", 0); err == nil {
		t.Fatal("short packet parsed without error")
	}
}
