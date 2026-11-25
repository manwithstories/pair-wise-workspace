package ts

import (
	"strings"
	"testing"

	"cinepipe/internal/fault"
)

func mkPacket(pid uint16, cc uint8, pusi bool, afc uint8, tail []byte) []byte {
	b := make([]byte, PacketSize)
	b[0] = SyncByte
	if pusi {
		b[1] |= 0x40
	}
	b[1] |= uint8(pid >> 8) & 0x1F
	b[2] = uint8(pid)
	b[3] = afc<<4 | cc&0x0F
	copy(b[4:], tail)
	for i := 4 + len(tail); i < PacketSize; i++ {
		b[i] = 0xFF // MPEG-2 stuffing after the last section
	}
	return b
}

func TestParseBasicPayloadPacket(t *testing.T) {
	raw := mkPacket(0x100, 5, true, 1, []byte{0xDE, 0xAD})
	p, errs := ParsePacket(raw, 0, 0)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if p.PID != 0x100 || p.CC != 5 || !p.PUSI || !p.HasPayload || p.HasAdaptation {
		t.Fatalf("bad packet: %+v", p)
	}
	if len(p.Payload) != PacketSize-4 {
		t.Fatalf("payload len %d", len(p.Payload))
	}
	if p.Payload[0] != 0xDE || p.Payload[1] != 0xAD {
		t.Fatal("payload content")
	}
}

func TestParseAdaptationPCR(t *testing.T) {
	af := []byte{MaxAdaptationLen, 0x10}
	for len(af) < MaxAdaptationLen+1 {
		af = append(af, 0xFF)
	}
	raw := mkPacket(0x1FF, 0, false, 2, af)
	p, errs := ParsePacket(raw, 188, 1)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if !p.HasAdaptation || p.HasPayload {
		t.Fatal("adaptation flags")
	}
	if !p.CarriesPCR() || len(p.Adaptation.Raw) != MaxAdaptationLen-1 {
		t.Fatalf("pcr fields: %+v", p.Adaptation)
	}
}

func TestParseShortPacket(t *testing.T) {
	raw := mkPacket(0x100, 1, false, 1, []byte{1, 2, 3})[:100]
	p, errs := ParsePacket(raw, 0, 0)
	if !p.Short {
		t.Fatal("should be short")
	}
	if len(errs) == 0 || errs[0].Kind != fault.KindTruncated {
		t.Fatalf("expected truncation, got %v", errs)
	}
	if len(p.Payload) != 96 {
		t.Fatalf("payload len %d", len(p.Payload))
	}
}

func TestParseBadSyncAndReservedAFC(t *testing.T) {
	bad := make([]byte, PacketSize)
	bad[0] = 0x00
	if _, errs := ParsePacket(bad, 0, 0); len(errs) != 1 || errs[0].Kind != fault.KindSync {
		t.Fatalf("sync errors: %v", errs)
	}
	reserved := mkPacket(0x100, 0, false, 0, nil)
	if _, errs := ParsePacket(reserved, 0, 0); len(errs) != 1 || errs[0].Kind != fault.KindAdaptation {
		t.Fatalf("afc errors: %v", errs)
	}
}

func TestParseAdaptationOverrun(t *testing.T) {
	// Length claims 60 bytes but a full 188-byte packet only leaves 183; ask
	// for more than the maximum to force the overrun branch.
	raw := mkPacket(0x100, 0, false, 2, []byte{200})
	if _, errs := ParsePacket(raw, 0, 0); len(errs) == 0 || errs[0].Kind != fault.KindAdaptation {
		t.Fatalf("expected adaptation error, got %v", errs)
	}
}

func TestContinuityCleanAndJump(t *testing.T) {
	ct := NewContinuityTracker()
	for i := 0; i < 5; i++ {
		p, _ := ParsePacket(mkPacket(0x100, uint8(i), false, 1, []byte{0}), int64(i*188), int64(i))
		if errs := ct.Observe(p); len(errs) != 0 {
			t.Fatalf("packet %d: %v", i, errs)
		}
	}
	// Lose two packets: 4 -> 7
	p, _ := ParsePacket(mkPacket(0x100, 7, false, 1, []byte{0}), 5*188, 5)
	errs := ct.Observe(p)
	if len(errs) != 1 || errs[0].Kind != fault.KindContinuity {
		t.Fatalf("expected jump: %v", errs)
	}
	if !strings.Contains(errs[0].Detail, "lost 2 packet") {
		t.Fatalf("detail %q", errs[0].Detail)
	}
}

func TestContinuityDuplicate(t *testing.T) {
	ct := NewContinuityTracker()
	p5, _ := ParsePacket(mkPacket(0x100, 5, false, 1, nil), 0, 0)
	ct.Observe(p5)
	p5dup, _ := ParsePacket(mkPacket(0x100, 5, false, 1, nil), 188, 1)
	errs := ct.Observe(p5dup)
	if len(errs) != 1 || !strings.Contains(errs[0].Detail, "duplicate") {
		t.Fatalf("expected duplicate: %v", errs)
	}
}

func TestContinuityDiscontinuityIndicator(t *testing.T) {
	ct := NewContinuityTracker()
	ct.Observe(mustPacket(t, mkPacket(0x100, 3, false, 1, nil), 0, 0))
	// A packet with adaptation discontinuity_indicator + payload at cc 9.
	af := make([]byte, MaxAdaptationLen+1)
	af[0] = MaxAdaptationLen
	af[1] = 0x80
	for i := 2; i < len(af); i++ {
		af[i] = 0xFF
	}
	disc, _ := ParsePacket(mkPacket(0x100, 9, false, 3, af), 188, 1)
	if errs := ct.Observe(disc); len(errs) != 0 {
		t.Fatalf("discontinuity should be accepted: %v", errs)
	}
}

func mustPacket(t *testing.T, raw []byte, off, idx int64) *Packet {
	t.Helper()
	p, errs := ParsePacket(raw, off, idx)
	if len(errs) != 0 {
		t.Fatalf("parse: %v", errs)
	}
	return p
}
