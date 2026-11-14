package psi

import "testing"

func TestBuildPATCRCRoundTrip(t *testing.T) {
	s := BuildPAT(1, 0, true, []PATBuildProgram{{ProgramNumber: 9000, PID: 0x1f0}, {ProgramNumber: 1, PID: 0x100}})
	if err := verifySectionCRC(s); err != nil {
		t.Fatalf("CRC mismatch: %v raw=%x", err, s)
	}
	sec, err := parseSection(0, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePAT(sec)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 0 || len(p.Programs) != 2 || p.Programs[0].PID != 0x1f0 {
		t.Fatalf("unexpected PAT: %+v", p)
	}
}
