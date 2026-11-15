package psi

import "testing"

func withCRC(sec []byte) []byte {
	c := CRC32(sec)
	return append(sec, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
}

func TestParsePAT(t *testing.T) {
	sec := []byte{0x00, 0xB0, 0x15, 0x00, 0x01, 0xC1 | (7 << 1), 0x00, 0x00}
	sec = append(sec, 0x00, 0x00, 0xE0, 0x10) // network PID, skipped
	sec = append(sec, 0x00, 0x01, 0xE1, 0x01) // program 1 -> 0x101
	sec = append(sec, 0x00, 0x02, 0xE1, 0x02) // program 2 -> 0x102
	pat, err := ParsePAT(withCRC(sec))
	if err != nil {
		t.Fatal(err)
	}
	if pat.Version != 7 || !pat.Current {
		t.Fatalf("version %d current %v", pat.Version, pat.Current)
	}
	if len(pat.Entries) != 2 || pat.Entries[0].PMTPID != 0x101 || pat.Entries[1].Program != 2 {
		t.Fatalf("entries %+v", pat.Entries)
	}
}

func TestParsePMT(t *testing.T) {
	sec := []byte{0x02, 0xB0, 0x00, 0x00, 0x01, 0xC1 | (3 << 1), 0x00, 0x00,
		0xE2, 0x10, 0xF0, 0x00}
	// video stream, no descriptors
	sec = append(sec, 0x1B, 0xE2, 0x10, 0xF0, 0x00)
	// audio stream with ISO639 + registration descriptors
	desc := []byte{0x0A, 0x04, 'e', 'n', 'g', 0x00, 0x05, 0x04, 'A', 'C', '-', '3'}
	sec = append(sec, 0x0F, 0xE2, 0x11, 0xF0, byte(len(desc)))
	sec = append(sec, desc...)
	sec[2] = byte(len(sec) - 3 + 4) // section_length incl. CRC
	pmt, err := ParsePMT(withCRC(sec))
	if err != nil {
		t.Fatal(err)
	}
	if pmt.Program != 1 || pmt.Version != 3 || pmt.PCRPID != 0x210 {
		t.Fatalf("pmt %+v", pmt)
	}
	if len(pmt.Streams) != 2 {
		t.Fatalf("streams %+v", pmt.Streams)
	}
	a := pmt.Streams[1]
	if a.Type != 0x0F || a.PID != 0x211 || a.Lang != "eng" || a.Registration != "AC-3" {
		t.Fatalf("audio stream %+v", a)
	}
}

func TestParseRejectsForeignTables(t *testing.T) {
	if _, err := ParsePAT(buildSection(0x02, 9)); err == nil {
		t.Fatal("PMT accepted as PAT")
	}
	if _, err := ParsePMT(buildSection(0x00, 9)); err == nil {
		t.Fatal("PAT accepted as PMT")
	}
}
