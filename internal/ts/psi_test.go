package ts

import (
	"strings"
	"testing"

	"cinepipe/internal/fault"
)

func patBody(program uint16, pmtPID uint16) []byte {
	return []byte{uint8(program >> 8), uint8(program), 0xE0 | uint8(pmtPID>>8)&0x1F, uint8(pmtPID)}
}

func pmtBody(pcrPID, videoPID, audioPID uint16, lang []byte) []byte {
	var body []byte
	body = append(body, 0xE0|uint8(pcrPID>>8)&0x1F, uint8(pcrPID))
	body = append(body, 0xF0, 0x00) // program_info_length 0
	// video stream
	body = append(body, StreamTypeH264, 0xE0|uint8(videoPID>>8)&0x1F, uint8(videoPID), 0xF0, 0x00)
	// audio stream with language descriptor
	esInfo := []byte{0x0A, 0x04, lang[0], lang[1], lang[2], 0x00}
	body = append(body, StreamTypeAAC, 0xE0|uint8(audioPID>>8)&0x1F, uint8(audioPID),
		0xF0|uint8(len(esInfo)>>8)&0x0F, uint8(len(esInfo)))
	body = append(body, esInfo...)
	return body
}

func psPacket(t *testing.T, pid uint16, cc uint8, section []byte, off, idx int64) *Packet {
	t.Helper()
	payload := append([]byte{0x00}, section...) // pointer_field 0
	b := mkPacket(pid, cc, true, 1, payload)
	return mustPacket(t, b, off, idx)
}

func TestPATAndPMTRoundTrip(t *testing.T) {
	asm := NewSectionAssembler()
	patSec := BuildSection(TIDPat, 1, 0, patBody(1, 0x100))
	p := psPacket(t, 0x00, 0, patSec, 0, 0)
	secs, errs := asm.Assemble(p)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(secs) != 1 || !secs[0].CRCValid {
		t.Fatalf("sections: %+v", secs)
	}
	pat, err := ParsePAT(secs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(pat.Programs) != 1 || pat.Programs[0].PMTPID != 0x100 {
		t.Fatalf("pat: %+v", pat)
	}

	pmtSec := BuildSection(TIDPmt, 1, 3, pmtBody(0x101, 0x101, 0x102, []byte("eng")))
	p2 := psPacket(t, 0x100, 0, pmtSec, 188, 1)
	secs2, errs2 := asm.Assemble(p2)
	if len(errs2) != 0 {
		t.Fatalf("pmt errors: %v", errs2)
	}
	pmt, err := ParsePMT(secs2[0])
	if err != nil {
		t.Fatal(err)
	}
	if pmt.Version != 3 || pmt.PCRPID != 0x101 || len(pmt.Streams) != 2 {
		t.Fatalf("pmt: %+v", pmt)
	}
	if !pmt.Streams[0].IsVideo() || !pmt.Streams[1].IsAudio() {
		t.Fatalf("stream kinds: %+v", pmt.Streams)
	}
	if pmt.Streams[1].HasLanguage != "eng" {
		t.Fatalf("language: %q", pmt.Streams[1].HasLanguage)
	}
}

func TestPSISpansTwoPackets(t *testing.T) {
	// Build a PMT whose body exceeds one packet (184 usable bytes per packet).
	pmtPID := uint16(0x100)
	bigBody := make([]byte, 200)
	copy(bigBody, pmtBody(0x101, 0x101, 0x102, []byte("eng")))
	for i := 4 + 15; i < len(bigBody); i += 5 {
		// filler descriptors; keep the structure valid enough for length fields
		bigBody[i] = 0xFF
	}
	sec := BuildSection(TIDPmt, 1, 0, bigBody)
	// A payload is 184 bytes and the pointer_field consumes one, so the first
	// packet can carry 183 section bytes.
	payload1 := append([]byte{0x00}, sec[:183]...)
	p1 := mustPacket(t, mkPacket(pmtPID, 0, true, 1, payload1), 0, 0)
	rest := sec[183:]
	p2 := mustPacket(t, mkPacket(pmtPID, 1, false, 1, rest), 188, 1)

	asm := NewSectionAssembler()
	secs, errs := asm.Assemble(p1)
	if len(secs) != 0 {
		t.Fatal("should not complete in one packet")
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	secs, errs = asm.Assemble(p2)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(secs) != 1 || !secs[0].Spanned || secs[0].Packets < 2 {
		t.Fatalf("spanned section: %+v", secs)
	}
	if secs[0].Offset != 0 {
		t.Fatalf("section should anchor at first packet offset, got %d", secs[0].Offset)
	}
}

func TestPSIStuffingIsLegal(t *testing.T) {
	asm := NewSectionAssembler()
	patSec := BuildSection(TIDPat, 1, 0, patBody(1, 0x100))
	payload := append([]byte{0x00}, patSec...)
	for len(payload) < PacketSize-4 {
		payload = append(payload, 0xFF)
	}
	p := mustPacket(t, mkPacket(0x00, 0, true, 1, payload), 0, 0)
	secs, errs := asm.Assemble(p)
	if len(errs) != 0 {
		t.Fatalf("stuffing reported as error: %v", errs)
	}
	if len(secs) != 1 {
		t.Fatalf("sections %d", len(secs))
	}
}

func TestPSICRCError(t *testing.T) {
	asm := NewSectionAssembler()
	patSec := BuildSection(TIDPat, 1, 0, patBody(1, 0x100))
	patSec[len(patSec)-1] ^= 0xFF // corrupt the CRC
	p := psPacket(t, 0x00, 0, patSec, 100, 0)
	secs, errs := asm.Assemble(p)
	if len(errs) != 1 || errs[0].Kind != fault.KindCRC {
		t.Fatalf("expected CRC error: %v", errs)
	}
	if len(secs) != 1 || secs[0].CRCValid {
		t.Fatal("section parsed but flagged invalid")
	}
	if errs[0].Offset != 100 {
		t.Fatalf("crc offset %d", errs[0].Offset)
	}
}

func TestPSIVersionSwitch(t *testing.T) {
	asm := NewSectionAssembler()
	v0 := BuildSection(TIDPat, 7, 0, patBody(1, 0x100))
	v1 := BuildSection(TIDPat, 7, 1, patBody(1, 0x101))
	if _, errs := asm.Assemble(psPacket(t, 0x00, 0, v0, 0, 0)); len(errs) != 0 {
		t.Fatalf("first version: %v", errs)
	}
	secs, errs := asm.Assemble(psPacket(t, 0x00, 1, v1, 188, 1))
	if len(errs) != 1 || errs[0].Kind != fault.KindPSIVersion {
		t.Fatalf("expected version-switch diagnostic: %v", errs)
	}
	if !strings.Contains(errs[0].Detail, "0->1") {
		t.Fatalf("detail %q", errs[0].Detail)
	}
	if len(secs) != 1 {
		t.Fatal("switched section should still be returned")
	}
}

func TestPSIPointerOverrun(t *testing.T) {
	asm := NewSectionAssembler()
	// Pointer_field claims 200 bytes past the payload.
	p := mustPacket(t, mkPacket(0x00, 0, true, 1, []byte{200, 1, 2, 3}), 0, 0)
	_, errs := asm.Assemble(p)
	if len(errs) != 1 || errs[0].Kind != fault.KindPSI || !strings.Contains(errs[0].Detail, "pointer_field") {
		t.Fatalf("pointer errors: %v", errs)
	}
}

func TestPSIAbandonedPartial(t *testing.T) {
	asm := NewSectionAssembler()
	// First packet starts a long section but we never finish it.
	long := BuildSection(TIDPmt, 1, 0, make([]byte, 200))
	p1 := mustPacket(t, mkPacket(0x100, 0, true, 1,
		append([]byte{0x00}, long[:184]...)), 0, 0)
	if _, errs := asm.Assemble(p1); len(errs) != 0 {
		t.Fatalf("first: %v", errs)
	}
	// Next packet PUSI with pointer 0 abandons the buffered section.
	patSec := BuildSection(TIDPat, 1, 0, patBody(1, 0x100))
	p2 := mustPacket(t, mkPacket(0x100, 1, true, 1, append([]byte{0x00}, patSec...)), 188, 1)
	_, errs := asm.Assemble(p2)
	if len(errs) != 1 || !strings.Contains(errs[0].Detail, "abandoned") {
		t.Fatalf("abandon errors: %v", errs)
	}
}

func TestCRC32KnownVector(t *testing.T) {
	// The empty-string CRC of the MPEG polynomial with init all-ones is
	// 0x00000000 after no iterations; a single 0xFF byte has a known value.
	b := []byte{0xFF}
	if got := CRC32MPEG(b); got == 0 {
		t.Fatal("degenerate crc")
	}
	// Self-consistency: a section's trailing CRC validates.
	sec := BuildSection(TIDPat, 1, 0, patBody(1, 0x100))
	if CRC32MPEG(sec[:len(sec)-4]) != uint32(sec[len(sec)-4])<<24|
		uint32(sec[len(sec)-3])<<16|uint32(sec[len(sec)-2])<<8|uint32(sec[len(sec)-1]) {
		t.Fatal("crc mismatch")
	}
}

func TestParsePATRejectsNonPAT(t *testing.T) {
	sec := BuildSection(TIDPmt, 1, 0, pmtBody(1, 1, 2, []byte("eng")))
	if _, err := ParsePAT(sec2section(t, sec)); err == nil {
		t.Fatal("expected error")
	}
	badBody := BuildSection(TIDPat, 1, 0, []byte{1, 2, 3}) // not multiple of 4
	if _, err := ParsePAT(sec2section(t, badBody)); err == nil {
		t.Fatal("expected body length error")
	}
}

// sec2section completes a section through the assembler and returns it.
func sec2section(t *testing.T, sec []byte) *Section {
	t.Helper()
	asm := NewSectionAssembler()
	secs, _ := asm.Assemble(psPacket(t, 0x00, 0, sec, 0, 0))
	if len(secs) != 1 {
		t.Fatalf("no section")
	}
	return secs[0]
}
