package rtp

import (
	"testing"

	pionrtp "github.com/pion/rtp"

	"avpipe/internal/media"
)

func mediaPkt(seq uint16, ts uint32, payload byte) *pionrtp.Packet {
	return &pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, PayloadType: PayloadTypeMedia, SequenceNumber: seq, Timestamp: ts, SSRC: SSRC, Marker: true},
		Payload: []byte{payload, payload, payload, payload},
	}
}

func TestSimulateNetworkDeterministic(t *testing.T) {
	pkts := make([]*pionrtp.Packet, 40)
	for i := range pkts {
		pkts[i] = mediaPkt(uint16(i), uint32(i)*3000, byte(i))
	}
	cfg := NetworkConfig{LossRate: 0.1, Burst: 2, JitterTicks: 5, Seed: 7}
	a := SimulateNetwork(pkts, cfg)
	b := SimulateNetwork(pkts, cfg)
	lossA, lossB := 0, 0
	for i := range a {
		if a[i].Lost != b[i].Lost || a[i].Arrive90k != b[i].Arrive90k {
			t.Fatalf("non-deterministic simulation at %d", i)
		}
		if a[i].Lost {
			lossA++
		}
		if b[i].Lost {
			lossB++
		}
	}
	if lossA == 0 || lossA != lossB {
		t.Fatalf("expected reproducible losses, got %d/%d", lossA, lossB)
	}
}

func TestJitterReorders(t *testing.T) {
	jb := NewJitterBuffer(64, 9000)
	in := []*pionrtp.Packet{mediaPkt(0, 0, 0), mediaPkt(2, 6000, 2), mediaPkt(1, 3000, 1)}
	for _, p := range in {
		if err := jb.Push(p); err != nil {
			t.Fatal(err)
		}
	}
	out := jb.Drain(9000)
	if len(out) != 3 {
		t.Fatalf("playouts=%d, want 3", len(out))
	}
	for i, po := range out {
		if po.Gap || po.Seq != uint16(i) {
			t.Fatalf("out[%d]=%+v, want in-order seq %d", i, po, i)
		}
	}
}

func TestJitterGapAfterWindow(t *testing.T) {
	jb := NewJitterBuffer(64, 1000)
	if err := jb.Push(mediaPkt(0, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := jb.Push(mediaPkt(2, 6000, 2)); err != nil {
		t.Fatal(err)
	}
	// First drain plays seq0 and arms a 1000-tick window for missing seq1.
	if out := jb.Drain(5000); len(out) != 1 || out[0].Seq != 0 {
		t.Fatalf("premature drain: %+v", out)
	}
	// Before the window (5000+1000=6000) seq1 still waits.
	if out := jb.Drain(5500); len(out) != 0 {
		t.Fatalf("seq1 should still be waiting: %+v", out)
	}
	// Past the window: seq1 surfaces as a gap, then seq2 plays.
	out := jb.Drain(6001)
	if len(out) != 2 || !out[0].Gap || out[0].Seq != 1 || out[1].Seq != 2 {
		t.Fatalf("gap handling wrong: %+v", out)
	}
}

func TestJitterCapacityLimit(t *testing.T) {
	jb := NewJitterBuffer(2, 9000)
	// Three distinct forward sequences all buffered without a Drain, so the
	// third Push exceeds the capacity.
	if err := jb.Push(mediaPkt(0, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := jb.Push(mediaPkt(5, 15000, 5)); err != nil {
		t.Fatal(err)
	}
	err := jb.Push(mediaPkt(6, 18000, 6))
	if err == nil {
		t.Fatal("expected limit-exceeded error")
	}
	if me, ok := err.(*media.Error); !ok || me.Kind != media.KindLimitExceeded {
		t.Fatalf("want locatable limit error, got %v", err)
	}
}

func TestPacketizeSkipsCorrupt(t *testing.T) {
	frames := []media.Frame{
		{Sequence: 0, PTS90k: 0, Payload: []byte{1}},
		{Sequence: 1, PTS90k: 3000, Corrupt: true, GapReason: media.KindChecksum},
		{Sequence: 2, PTS90k: 6000, Payload: []byte{2}},
	}
	pkts := Packetize(frames, 3000)
	if len(pkts) != 2 {
		t.Fatalf("packets=%d, want 2 (corrupt skipped)", len(pkts))
	}
}
