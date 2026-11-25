package ts

import "testing"

// TestPSIPacketAccounting verifies that a section spanning packets counts the
// packets that contributed to it, anchoring at the first one.
func TestPSIPacketAccounting(t *testing.T) {
	bigBody := make([]byte, 200)
	sec := BuildSection(TIDPmt, 1, 0, bigBody)
	if want := 3 + 5 + len(bigBody) + 4; want != len(sec) {
		t.Fatalf("section size %d want %d", len(sec), want)
	}
	// One payload is 184 bytes; the pointer_field takes one, so 183 section
	// bytes fit before the second packet carries the rest.
	p1 := mustPacket(t, mkPacket(0x100, 0, true, 1, append([]byte{0}, sec[:183]...)), 0, 0)
	asm := NewSectionAssembler()
	secs, errs := asm.Assemble(p1)
	if len(secs) != 0 || len(errs) != 0 {
		t.Fatalf("first packet: secs=%d errs=%v", len(secs), errs)
	}
	if part := asm.partial[0x100]; part == nil || part.packets != 1 {
		t.Fatalf("partial accounting: %+v", part)
	}

	p2 := mustPacket(t, mkPacket(0x100, 1, false, 1, sec[183:]), 188, 1)
	secs, errs = asm.Assemble(p2)
	if len(errs) != 0 {
		t.Fatalf("second packet errors: %v", errs)
	}
	if len(secs) != 1 {
		t.Fatalf("sections %d", len(secs))
	}
	s := secs[0]
	if !s.Spanned || s.Packets != 2 {
		t.Fatalf("spanned=%v packets=%d", s.Spanned, s.Packets)
	}
	if s.Offset != 0 {
		t.Fatalf("offset %d want 0", s.Offset)
	}
	if !s.CRCValid {
		t.Fatal("reassembled section CRC should validate")
	}
}
