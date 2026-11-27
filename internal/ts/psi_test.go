package ts

import (
	"testing"

	"tsqc/internal/clock"
)

func TestCRCRejectsCorruptPSI(t *testing.T) {
	sec := patSection(0x1000, 0)
	sec[len(sec)-1] ^= 0xff
	if _, err := ParseSection(sec); !clock.IsCode(err, clock.CodePSIInvalid) {
		t.Fatalf("want CRC error, got %v", err)
	}
	sec = patSection(0x1000, 0)
	s, err := ParseSection(sec)
	if err != nil {
		t.Fatal(err)
	}
	pat, err := ParsePAT(s)
	if err != nil || pat.Programs[1] != 0x1000 {
		t.Fatalf("pat=%+v err=%v", pat, err)
	}
}

func TestTableSwitchOnlyOnNewVersionAndValidCRC(t *testing.T) {
	tm := NewTableManager()
	s, _ := ParseSection(patSection(0x1100, 0))
	if changed, err := tm.Apply(s); err != nil || !changed {
		t.Fatalf("apply %v %v", changed, err)
	}
	same, _ := ParseSection(patSection(0x1200, 0))
	if changed, err := tm.Apply(same); err != nil || changed {
		t.Fatalf("same version must not switch: %v %v", changed, err)
	}
	if tm.PAT().Programs[1] != 0x1100 {
		t.Fatal("table mutated on duplicate version")
	}
	next, _ := ParseSection(patSection(0x1200, 1))
	if changed, err := tm.Apply(next); err != nil || !changed {
		t.Fatalf("version bump: %v %v", changed, err)
	}
	if tm.PAT().Programs[1] != 0x1200 {
		t.Fatal("version bump not applied")
	}
}
