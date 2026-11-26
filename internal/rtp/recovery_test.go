package rtp_test

import (
	"testing"

	mrtp "medialab/internal/rtp"
)

func TestXORRecoversSoleMissingMember(t *testing.T) {
	a := testPacket(1, 0, "aaaa", 0)
	b := testPacket(2, 0, "bbbb", 64)
	group, err := mrtp.BuildXORGroup("pair", a, b)
	if err != nil {
		t.Fatal(err)
	}
	available := map[uint16]*mrtp.InPacket{1: a}
	got, ok := group.Recover(available)
	if !ok {
		t.Fatal("XOR recovery failed")
	}
	if got.P.SequenceNumber != 2 || string(got.P.Payload) != "bbbb" || got.RawOffset != 64 {
		t.Fatalf("recovered=%+v", got)
	}
}

func TestRetransmitCacheEnforcesTimeLimitAndBound(t *testing.T) {
	c := mrtp.NewRetransmitCache(80, 2)
	p1 := testPacket(1, 0, "one", 0)
	p2 := testPacket(2, 0, "two", 64)
	p3 := testPacket(3, 0, "three", 128)
	c.Store(0, p1)
	c.Store(0, p2)
	c.Store(0, p3) // evicts sequence 1 by FIFO bound.
	if _, ok := c.Get(1, 10); ok {
		t.Fatal("sequence 1 should have been evicted")
	}
	if _, ok := c.Get(2, 80); !ok {
		t.Fatal("sequence 2 should be available at TTL boundary")
	}
	if _, ok := c.Get(2, 81); ok {
		t.Fatal("sequence 2 should be expired after TTL")
	}
}

func TestEnginePrefersRetransmissionThenXOR(t *testing.T) {
	engine := mrtp.NewEngine(10)
	expired := testPacket(1, 0, "expired", 0)
	partner := testPacket(3, 0, "partner", 128)
	recent := testPacket(2, 0, "recent", 64)

	group, err := mrtp.BuildXORGroup("g", expired, partner)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.FEC.Add(group); err != nil {
		t.Fatal(err)
	}
	engine.Cache.Store(0, expired)
	engine.Cache.Store(95, recent)

	available := map[uint16]*mrtp.InPacket{3: partner}
	results := engine.RecoverLost(100, []uint16{1, 2}, available)
	if len(results) != 2 {
		t.Fatalf("results=%d", len(results))
	}
	if results[0].State != "xor_recovered" || string(results[0].Packet.P.Payload) != "expired" {
		t.Fatalf("first result=%+v", results[0])
	}
	if results[1].State != "retransmitted" || string(results[1].Packet.P.Payload) != "recent" {
		t.Fatalf("second result=%+v", results[1])
	}
	if engine.XORRecovered != 1 || engine.Retransmitted != 1 || engine.Unrecoverable != 0 {
		t.Fatalf("engine counters=%+v", engine)
	}
}
