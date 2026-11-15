package psi

import (
	"bytes"
	"testing"
)

// buildSection returns a CRC-valid dummy section of n payload bytes.
func buildSection(tableID byte, n int) []byte {
	sec := make([]byte, 3+n)
	sec[0] = tableID
	sec[1] = 0xB0 | byte((n+4)>>8) // section_length includes the CRC
	sec[2] = byte(n + 4)
	for i := 3; i < len(sec); i++ {
		sec[i] = byte(i)
	}
	c := CRC32(sec)
	return append(sec, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
}

func collect(a *Assembler, pusi bool, payload []byte) [][]byte {
	var out [][]byte
	a.Write(pusi, payload, func(sec []byte) {
		cp := append([]byte(nil), sec...)
		out = append(out, cp)
	})
	return out
}

func TestCRCKnownVector(t *testing.T) {
	// MPEG-2 CRC32 of "123456789" is 0x0376E6E7.
	if c := CRC32([]byte("123456789")); c != 0x0376E6E7 {
		t.Fatalf("crc=%08X", c)
	}
	sec := buildSection(0x00, 9)
	if CRC32(sec) != 0 {
		t.Fatal("section with CRC must check to 0")
	}
}

func TestTwoSectionsOnePacket(t *testing.T) {
	s1, s2 := buildSection(0x00, 5), buildSection(0x02, 7)
	payload := append([]byte{0x00}, s1...)
	payload = append(payload, s2...)
	payload = append(payload, 0xFF, 0xFF) // stuffing
	a := &Assembler{}
	got := collect(a, true, payload)
	if len(got) != 2 || !bytes.Equal(got[0], s1) || !bytes.Equal(got[1], s2) {
		t.Fatalf("got %d sections", len(got))
	}
}

func TestSectionSpanningPackets(t *testing.T) {
	s := buildSection(0x02, 40)
	a := &Assembler{}
	p1 := append([]byte{0x00}, s[:10]...)
	if got := collect(a, true, p1); len(got) != 0 {
		t.Fatal("section not complete yet")
	}
	got := collect(a, false, s[10:])
	if len(got) != 1 || !bytes.Equal(got[0], s) {
		t.Fatalf("got %d sections", len(got))
	}
}

func TestPointerFieldCompletesPrevious(t *testing.T) {
	s1, s2 := buildSection(0x00, 9), buildSection(0x00, 6)
	a := &Assembler{}
	// First packet: pointer 0, start of s1 (5 of 16 bytes).
	collect(a, true, append([]byte{0x00}, s1[:5]...))
	// Second packet: pointer_field skips the remaining 11 bytes of s1,
	// then s2 starts in the same packet.
	payload := []byte{11}
	payload = append(payload, s1[5:]...)
	payload = append(payload, s2...)
	got := collect(a, true, payload)
	if len(got) != 2 || !bytes.Equal(got[0], s1) || !bytes.Equal(got[1], s2) {
		t.Fatalf("got %d sections", len(got))
	}
}

func TestCRCErrorDropped(t *testing.T) {
	s := buildSection(0x00, 5)
	s[4] ^= 0xFF // corrupt
	a := &Assembler{}
	got := collect(a, true, append([]byte{0x00}, s...))
	if len(got) != 0 || a.CRCErrors != 1 {
		t.Fatalf("got %d sections, crc errors %d", len(got), a.CRCErrors)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	s := buildSection(0x02, 500) // spans multiple TS packets
	pkts := MarshalSection(0x123, 3, s)
	if len(pkts)%188 != 0 {
		t.Fatal("packet size mismatch")
	}
	a := &Assembler{}
	var got [][]byte
	for i := 0; i+188 <= len(pkts); i += 188 {
		p := pkts[i : i+188]
		if p[0] != 0x47 || p[2] != 0x23 {
			t.Fatal("bad header")
		}
		got = append(got, collect(a, i == 0, p[4:])...)
	}
	if len(got) != 1 || !bytes.Equal(got[0], s) {
		t.Fatalf("got %d sections", len(got))
	}
}
