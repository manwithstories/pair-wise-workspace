package fanout

import (
	"testing"

	"github.com/pion/rtp"

	mrtp "mediad/internal/rtp"
)

func TestLadderShape(t *testing.T) {
	rungs := Ladder(8_000_000, 3)
	if len(rungs) != 3 {
		t.Fatalf("rungs=%d", len(rungs))
	}
	if rungs[0].Bitrate != 8_000_000 || rungs[1].Bitrate != 4_000_000 || rungs[2].Bitrate != 2_000_000 {
		t.Fatalf("rungs=%+v", rungs)
	}
	if got := len(Ladder(1_000_000, 1)); got != 3 {
		t.Fatalf("n clamped to 3, got %d", got)
	}
	if got := len(Ladder(1_000_000, 99)); got != 8 {
		t.Fatalf("n clamped to 8, got %d", got)
	}
}

func TestFanOutRecoversEachRung(t *testing.T) {
	var seq uint16 = 1
	var pkts []rtp.Packet
	for i := 0; i < 60; i++ {
		pkts = append(pkts, mrtp.Packetize([]byte{byte(i), 0x80}, 9, &seq, uint32(i*3600), 1200)...)
	}
	results := FanOut(pkts, Ladder(6_000_000, 3), 8, pkts)
	if len(results) != 3 {
		t.Fatalf("layers=%d", len(results))
	}
	for _, lr := range results {
		if len(lr.Packets) != len(pkts) {
			t.Fatalf("rung %s released %d of %d", lr.Rung.Name, len(lr.Packets), len(pkts))
		}
		if lr.Stats.RecoveryRate() < 0.9 {
			t.Fatalf("rung %s rate=%v", lr.Rung.Name, lr.Stats.RecoveryRate())
		}
	}
	// Rung 0 must have seen the injected burst.
	if results[0].Stats.Lost == 0 {
		t.Fatal("rung 0 saw no loss; burst not injected")
	}
}
