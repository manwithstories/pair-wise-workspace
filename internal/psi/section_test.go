package psi

import (
	"testing"
)

func TestAssemblerSamePacketAndPointerTail(t *testing.T) {
	first, err := BuildPAT(1, 0, []PATProgram{{Program: 1, PID: 0x100}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildPMT(1, 0x100, 0, []ESInfo{{StreamType: 0x1b, ElementaryPID: 0x200}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAssembler()
	payload := append([]byte{0}, append(first, second...)...)
	sections := a.Feed(0, true, payload)
	if len(sections) != 2 || !sections[0].CRCValid || !sections[1].CRCValid {
		t.Fatalf("same-packet sections=%d, want 2 valid", len(sections))
	}

	a = NewAssembler()
	if got := a.Feed(0, true, append([]byte{0}, first[:10]...)); len(got) != 0 {
		t.Fatalf("unexpected section before tail completion: %d", len(got))
	}
	tail := first[10:]
	pointerPayload := append([]byte{byte(len(tail))}, tail...)
	pointerPayload = append(pointerPayload, second...)
	sections = a.Feed(0, true, pointerPayload)
	if len(sections) != 2 || !sections[0].CRCValid || !sections[1].CRCValid {
		t.Fatalf("pointer-tail sections=%d, want 2 valid", len(sections))
	}
}

func TestAssemblerCrossPacket(t *testing.T) {
	programs := make([]PATProgram, 60)
	for i := range programs {
		programs[i] = PATProgram{Program: uint16(i + 1), PID: uint16(0x100 + i)}
	}
	section, err := BuildPAT(1, 0, programs)
	if err != nil {
		t.Fatal(err)
	}
	packets := PacketizeSection(0, 0, section)
	if len(packets) < 2 {
		t.Fatalf("test section did not span packets: %d", len(packets))
	}
	a := NewAssembler()
	var sections []Section
	for i, packet := range packets {
		sections = append(sections, a.Feed(0, i == 0, packet[4:])...)
	}
	if len(sections) != 1 || !sections[0].CRCValid {
		t.Fatalf("cross-packet sections=%d, want one valid", len(sections))
	}
}

func TestAssemblerDropsBadCRC(t *testing.T) {
	section, err := BuildPAT(1, 0, []PATProgram{{Program: 1, PID: 0x100}})
	if err != nil {
		t.Fatal(err)
	}
	section[len(section)-1] ^= 0xff
	a := NewAssembler()
	sections := a.Feed(0, true, append([]byte{0}, section...))
	if len(sections) != 1 || sections[0].CRCValid {
		t.Fatalf("bad CRC section was not reported as invalid")
	}
	if a.CRCErrors != 1 {
		t.Fatalf("crc errors=%d, want 1", a.CRCErrors)
	}
}

func TestAssemblerLengthAndPointerErrors(t *testing.T) {
	a := NewAssembler()
	bad := []byte{0, 0x00, 0x00, 0x00}
	sections := a.Feed(0, true, bad)
	if len(sections) != 1 || sections[0].Err == "" || a.LengthErrors != 1 {
		t.Fatalf("invalid length was not rejected: %+v", sections)
	}
	sections = a.Feed(0, true, []byte{9, 1, 2})
	if len(sections) != 1 || sections[0].Err == "" || a.PointerErrors != 1 {
		t.Fatalf("invalid pointer was not rejected: %+v", sections)
	}
}

func TestCRCResidue(t *testing.T) {
	section, err := BuildPAT(1, 0, []PATProgram{{Program: 1, PID: 0x100}})
	if err != nil {
		t.Fatal(err)
	}
	if CRC32(section) != 0 {
		t.Fatalf("CRC residue=%#x, want zero", CRC32(section))
	}
}
