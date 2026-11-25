package fixture

import (
	"testing"

	"streamcut/internal/ts"
)

func TestGenerateCleanIsDeterministicAndOnRate(t *testing.T) {
	cfg := DefaultConfig(17, 4_000_000, CaseClean)
	r1, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Packets) != len(r2.Packets) {
		t.Fatal("same seed must give same packet count")
	}
	for i := range r1.Packets {
		if string(r1.Packets[i]) != string(r2.Packets[i]) {
			t.Fatalf("packet %d differs across runs", i)
		}
	}
	want := int64(4_000_000 * 10 / 8)
	off := int64(r1.Bytes) - want
	if off < 0 {
		off = -off
	}
	if pct := float64(off) * 100 / float64(want); pct > 1.0 {
		t.Fatalf("byte budget off by %.4f%%", pct)
	}
	if r1.Segments != 5 || len(r1.Silence) != 2 {
		t.Fatalf("segments=%d silence intervals=%d", r1.Segments, len(r1.Silence))
	}
	// Every emitted packet is exactly 188 bytes and sync-aligned.
	for i, p := range r1.Packets {
		if len(p) != ts.PacketSize || p[0] != 0x47 {
			t.Fatalf("packet %d malformed", i)
		}
	}
}

func TestWrapCaseCrossesModulus(t *testing.T) {
	cfg := DefaultConfig(17, 4_000_000, CaseWrap)
	r, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sawLow, sawHigh := false, false
	for _, p := range r.Packets {
		pkt, err := ts.Parse(p)
		if err != nil || !pkt.HasPCR {
			continue
		}
		base := pkt.PCR / 300
		if base < 90_000 {
			sawLow = true
		}
		if base > (1<<33)-90_000 {
			sawHigh = true
		}
	}
	if !sawLow || !sawHigh {
		t.Fatalf("wrap fixture must straddle 2^33: low=%v high=%v", sawLow, sawHigh)
	}
}
