package psi

import (
	"encoding/binary"
	"testing"

	"tsaudit/internal/tspipe"
)

func testSection(tableID uint8, extension uint16, version, number, last uint8, current bool, body []byte) Section {
	length := 5 + len(body) + 4
	raw := make([]byte, 3+length)
	raw[0] = tableID
	raw[1] = 0xb0 | byte(length>>8)
	raw[2] = byte(length)
	binary.BigEndian.PutUint16(raw[3:5], extension)
	raw[5] = 0xc0 | version<<1
	if current {
		raw[5] |= 1
	}
	raw[6], raw[7] = number, last
	copy(raw[8:], body)
	binary.BigEndian.PutUint32(raw[len(raw)-4:], CRC32MPEG(raw[:len(raw)-4]))
	sec, err := parseSection(raw, 0, 188)
	if err != nil {
		panic(err)
	}
	return *sec
}

func psPacket(offset int64, pusi bool, payload []byte) *tspipe.Packet {
	return &tspipe.Packet{Offset: offset, PID: 0, PUSI: pusi, HasPayload: true, Payload: payload}
}

func TestAssemblerOnePacketMultipleSections(t *testing.T) {
	s1 := testSection(TablePAT, 1, 3, 0, 1, true, []byte{0, 1, 0xe1, 0x00})
	s2 := testSection(TablePAT, 1, 3, 1, 1, true, []byte{0, 2, 0xe1, 0x01})
	payload := append([]byte{0}, s1.Raw...)
	payload = append(payload, s2.Raw...)
	out := NewAssembler().Feed(psPacket(0, true, payload))
	if len(out) != 2 || out[0].SectionNumber != 0 || out[1].SectionNumber != 1 {
		t.Fatalf("sections = %+v", out)
	}
}

func TestAssemblerCrossPacketAndNonZeroPointer(t *testing.T) {
	s1 := testSection(TablePAT, 1, 0, 0, 0, true, []byte{0, 1, 0xe1, 0x00})
	s2 := testSection(TablePAT, 1, 0, 0, 0, true, []byte{0, 2, 0xe1, 0x01})
	split := 7
	a := NewAssembler()
	if out := a.Feed(psPacket(0, true, append([]byte{0}, s1.Raw[:split]...))); len(out) != 0 {
		t.Fatalf("premature section: %+v", out)
	}
	rest := s1.Raw[split:]
	payload := append([]byte{byte(len(rest))}, rest...)
	payload = append(payload, s2.Raw...)
	out := a.Feed(psPacket(188, true, payload))
	if len(out) != 2 {
		t.Fatalf("got %d sections, malformed=%d", len(out), a.Malformed)
	}
	if out[0].Hash != s1.Hash || out[1].Hash != s2.Hash {
		t.Fatal("reassembled section hashes do not match")
	}
}

func TestAssemblerCRCFailureAndDeduplication(t *testing.T) {
	sec := testSection(TablePAT, 1, 0, 0, 0, true, []byte{0, 1, 0xe1, 0x00})
	bad := append([]byte{0}, sec.Raw...)
	bad[len(bad)-1] ^= 0xff
	a := NewAssembler()
	if out := a.Feed(psPacket(0, true, bad)); len(out) != 0 || a.CRCErrors != 1 {
		t.Fatalf("bad CRC was not discarded: out=%d errors=%d", len(out), a.CRCErrors)
	}
	good := append([]byte{0}, sec.Raw...)
	if out := a.Feed(psPacket(188, true, good)); len(out) != 1 {
		t.Fatalf("expected one section, got %d", len(out))
	}
	if out := a.Feed(psPacket(376, true, good)); len(out) != 0 || a.DuplicateSections != 1 {
		t.Fatalf("duplicate not suppressed: out=%d duplicates=%d", len(out), a.DuplicateSections)
	}
}

func TestCRC32MPEGValidSectionIsZero(t *testing.T) {
	sec := testSection(TablePAT, 1, 0, 0, 0, true, []byte{0, 1, 0xe1, 0x00})
	if CRC32MPEG(sec.Raw) != 0 {
		t.Fatalf("CRC check = %08x", CRC32MPEG(sec.Raw))
	}
}
