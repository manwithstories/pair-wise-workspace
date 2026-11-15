package program

import (
	"testing"

	"tsprobe/internal/psi"
)

func patSec(t *testing.T, version int, entries ...[2]uint16) []byte {
	sec := []byte{0x00, 0xB0, 0x00, 0x00, 0x01, 0xC1 | byte(version<<1), 0x00, 0x00}
	for _, e := range entries {
		sec = append(sec, byte(e[0]>>8), byte(e[0]), 0xE0|byte(e[1]>>8), byte(e[1]))
	}
	sec[2] = byte(len(sec) - 3 + 4)
	c := psi.CRC32(sec)
	return append(sec, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
}

func pmtSec(t *testing.T, prog uint16, version int, streams ...[2]uint16) []byte {
	sec := []byte{0x02, 0xB0, 0x00, byte(prog >> 8), byte(prog),
		0xC1 | byte(version<<1), 0x00, 0x00, 0xE2, 0x00, 0xF0, 0x00}
	for _, s := range streams {
		sec = append(sec, byte(s[0]), 0xE0|byte(s[1]>>8), byte(s[1]), 0xF0, 0x00)
	}
	sec[2] = byte(len(sec) - 3 + 4)
	c := psi.CRC32(sec)
	return append(sec, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
}

func TestVersionChangesAndWrap(t *testing.T) {
	s := NewState()
	e := [2]uint16{1, 0x101}
	for i, v := range []int{0, 0, 1, 1, 31, 0} {
		if err := s.FeedPAT(patSec(t, v, e), int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if s.PATChanges != 3 { // 0->1, 1->31, 31->0 wrap
		t.Fatalf("pat changes %d", s.PATChanges)
	}
	if len(s.Events) != 3 || s.Events[2].Old != 31 || s.Events[2].New != 0 {
		t.Fatalf("events %+v", s.Events)
	}
}

func TestPMTVersionsPerProgram(t *testing.T) {
	s := NewState()
	s.FeedPAT(patSec(t, 0, [2]uint16{1, 0x101}, [2]uint16{2, 0x102}), 0)
	s.FeedPMT(0x101, pmtSec(t, 1, 0, [2]uint16{0x1B, 0x210}), 1)
	s.FeedPMT(0x101, pmtSec(t, 1, 0, [2]uint16{0x1B, 0x210}), 2) // repeat
	s.FeedPMT(0x101, pmtSec(t, 1, 1, [2]uint16{0x1B, 0x210}), 3)
	s.FeedPMT(0x102, pmtSec(t, 2, 0, [2]uint16{0x1B, 0x220}), 4)
	if s.PMTChanges[1] != 1 || s.PMTChanges[2] != 0 {
		t.Fatalf("pmt changes %v", s.PMTChanges)
	}
	if len(s.Validate()) != 0 {
		t.Fatalf("validate %v", s.Validate())
	}
}

func TestPIDMapHistory(t *testing.T) {
	s := NewState()
	s.FeedPAT(patSec(t, 0, [2]uint16{1, 0x101}), 0)
	s.FeedPMT(0x101, pmtSec(t, 1, 0, [2]uint16{0x1B, 0x210}, [2]uint16{0x0F, 0x211}), 1)
	// Same audio PID, new stream type.
	s.FeedPMT(0x101, pmtSec(t, 1, 1, [2]uint16{0x1B, 0x210}, [2]uint16{0x04, 0x211}), 2)
	// PMT PID remapped by a new PAT version.
	s.FeedPAT(patSec(t, 1, [2]uint16{1, 0x121}), 3)
	s.FeedPMT(0x121, pmtSec(t, 1, 2, [2]uint16{0x1B, 0x210}, [2]uint16{0x04, 0x211}), 4)
	var notes []string
	for _, e := range s.PIDMap {
		notes = append(notes, e.Note)
	}
	want := []string{"new", "new", "type-changed", "pmt-moved", "pmt-moved"}
	if len(notes) != len(want) {
		t.Fatalf("notes %v", notes)
	}
	for i := range want {
		if notes[i] != want[i] {
			t.Fatalf("notes %v", notes)
		}
	}
	if s.PIDMap[4].PMTPID != 0x121 {
		t.Fatalf("entry %+v", s.PIDMap[4])
	}
}

func TestValidate(t *testing.T) {
	s := NewState()
	if len(s.Validate()) == 0 {
		t.Fatal("missing PAT must be reported")
	}
	s.FeedPAT(patSec(t, 0, [2]uint16{1, 0x101}), 0)
	problems := s.Validate()
	if len(problems) != 1 {
		t.Fatalf("validate %v", problems)
	}
	s.FeedPMT(0x101, pmtSec(t, 1, 0), 1)
	if len(s.Validate()) != 0 {
		t.Fatalf("validate %v", s.Validate())
	}
}
