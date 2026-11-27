package ts

import (
	"testing"

	"tsqc/internal/clock"
)

// --- tests ------------------------------------------------------------------

func TestParsePacketPCRAndContinuity(t *testing.T) {
	raw := tsPacket(0x100, 3, true, []byte{1, 2, 3}, 90000*300, true)
	p, err := ParsePacket(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.PID != 0x100 || !p.PUSI || p.CC != 3 || !p.PCRSet || p.PCR != 90000*300 {
		t.Fatalf("parsed %+v", p)
	}
	bad := append([]byte(nil), raw...)
	bad[0] = 0x48
	if _, err := ParsePacket(bad); !clock.IsCode(err, clock.CodeTSInvalidSync) {
		t.Fatalf("want sync error, got %v", err)
	}
}

func TestSplitPacketsTruncatedTail(t *testing.T) {
	raw := tsPacket(0x100, 0, false, []byte{1}, 0, false)
	buf := append(append([]byte{0xde, 0xad}, raw...), raw[:77]...)
	pkts, trunc, err := SplitPackets(buf)
	if len(pkts) != 1 {
		t.Fatalf("pkts=%d", len(pkts))
	}
	if trunc == 0 {
		t.Fatal("expected truncated bytes")
	}
	if !clock.IsCode(err, clock.CodeTruncated) {
		t.Fatalf("err=%v", err)
	}
}
