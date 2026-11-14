package psi

import (
	"encoding/binary"
	"testing"
)

func patBody(entries ...[2]uint16) []byte {
	body := make([]byte, 0, len(entries)*4)
	for _, e := range entries {
		b := make([]byte, 4)
		binary.BigEndian.PutUint16(b[0:2], e[0])
		binary.BigEndian.PutUint16(b[2:4], 0xe000|e[1])
		body = append(body, b...)
	}
	return body
}

func pmtBody(program, pcrPID, esPID uint16, streamType uint8) []byte {
	body := make([]byte, 4)
	binary.BigEndian.PutUint16(body[0:2], 0xe000|pcrPID)
	binary.BigEndian.PutUint16(body[2:4], 0xf000)
	body = append(body, streamType)
	var es [4]byte
	binary.BigEndian.PutUint16(es[0:2], 0xe000|esPID)
	binary.BigEndian.PutUint16(es[2:4], 0xf000)
	body = append(body, es[:]...)
	_ = program
	return body
}

func TestRegistryCandidateDoesNotActivate(t *testing.T) {
	r := NewRegistry(20)
	candidate := testSection(TablePAT, 1, 1, 0, 0, false, patBody([2]uint16{1, 0x100}))
	r.HandleSection(candidate)
	if r.CurrentSnapshot().ID != 0 {
		t.Fatalf("candidate activated early: snapshot %d", r.CurrentSnapshot().ID)
	}
	current := testSection(TablePAT, 1, 1, 0, 0, true, patBody([2]uint16{1, 0x100}))
	r.HandleSection(current)
	if r.CurrentSnapshot().ID == 0 || r.PATVersionChanges() != 0 {
		t.Fatalf("current table did not activate: snapshot=%d changes=%d",
			r.CurrentSnapshot().ID, r.PATVersionChanges())
	}
}

func TestRegistryVersionDedupAndWrap(t *testing.T) {
	r := NewRegistry(20)
	first := testSection(TablePAT, 1, 31, 0, 0, true, patBody([2]uint16{1, 0x100}))
	r.HandleSection(first)
	r.HandleSection(first)
	if r.PATVersionChanges() != 0 {
		t.Fatalf("same-version resend counted: %d", r.PATVersionChanges())
	}
	wrapped := testSection(TablePAT, 1, 0, 0, 0, true, patBody([2]uint16{1, 0x100}))
	r.HandleSection(wrapped)
	if r.PATVersionChanges() != 1 || r.CurrentSnapshot().PATVersion != 0 {
		t.Fatalf("wrapped version not activated correctly: changes=%d version=%d",
			r.PATVersionChanges(), r.CurrentSnapshot().PATVersion)
	}
	r.HandleSection(first)
	if r.PATVersionChanges() != 1 || r.CurrentSnapshot().PATVersion != 0 {
		t.Fatalf("stale retransmission rolled back active table: changes=%d version=%d",
			r.PATVersionChanges(), r.CurrentSnapshot().PATVersion)
	}
}

func TestRegistryPendingTimeout(t *testing.T) {
	r := NewRegistry(10)
	r.Tick(1)
	r.HandleSection(testSection(TablePAT, 1, 0, 0, 0, true, patBody([2]uint16{1, 0x100})))
	r.Tick(12)
	if len(r.Warnings) == 0 {
		t.Fatal("missing PMT did not produce a timeout warning")
	}
}

func TestRegistryPMTVersionFreezesAfterProgramDisappears(t *testing.T) {
	r := NewRegistry(20)
	r.HandleSection(testSection(TablePAT, 1, 0, 0, 0, true, patBody([2]uint16{1, 0x100})))
	r.HandleSection(testSection(TablePMT, 1, 0, 0, 0, true, pmtBody(1, 0x200, 0x201, 0x1b)))
	r.HandleSection(testSection(TablePMT, 1, 1, 0, 0, true, pmtBody(1, 0x200, 0x201, 0x1b)))
	if r.PMTVersionChanges()[1] != 1 {
		t.Fatalf("PMT changes before removal = %d", r.PMTVersionChanges()[1])
	}
	r.HandleSection(testSection(TablePAT, 1, 1, 0, 0, true, nil))
	r.HandleSection(testSection(TablePMT, 1, 2, 0, 0, true, pmtBody(1, 0x200, 0x201, 0x1b)))
	if r.PMTVersionChanges()[1] != 1 {
		t.Fatalf("removed program PMT version was not frozen: %d", r.PMTVersionChanges()[1])
	}
}

func TestRegistryStreamTypeAndPSIOverlapConflicts(t *testing.T) {
	r := NewRegistry(20)
	r.HandleSection(testSection(TablePAT, 1, 0, 0, 0, true, patBody([2]uint16{1, 0x100})))
	r.HandleSection(testSection(TablePMT, 1, 0, 0, 0, true, pmtBody(1, 0x200, 0x201, 0x1b)))
	r.HandleSection(testSection(TablePMT, 1, 1, 0, 0, true, pmtBody(1, 0x200, 0x201, 0x0f)))
	r.HandleSection(testSection(TablePMT, 1, 2, 0, 0, true, pmtBody(1, 0x200, 0x100, 0x0f)))
	if len(r.Conflicts) < 2 {
		t.Fatalf("expected stream-type and PSI-overlap conflicts, got %+v", r.Conflicts)
	}
	for _, c := range r.Conflicts {
		if c.SnapshotID == 0 {
			t.Fatalf("conflict missing snapshot id: %+v", c)
		}
	}
}

func TestRegistryPMTPIDRemapReturnsToPending(t *testing.T) {
	r := NewRegistry(20)
	r.HandleSection(testSection(TablePAT, 1, 0, 0, 0, true, patBody([2]uint16{1, 0x100})))
	r.HandleSection(testSection(TablePMT, 1, 0, 0, 0, true, pmtBody(1, 0x200, 0x201, 0x1b)))
	r.HandleSection(testSection(TablePAT, 1, 1, 0, 0, true, patBody([2]uint16{1, 0x101})))
	if countES(r.CurrentSnapshot()) != 0 {
		t.Fatalf("old PMT survived PID remap: %+v", r.CurrentSnapshot().Entries)
	}
	r.HandleSection(testSection(TablePMT, 1, 0, 0, 0, true, pmtBody(1, 0x200, 0x201, 0x1b)))
	if countES(r.CurrentSnapshot()) != 1 {
		t.Fatalf("same PMT content did not activate on new PID: %+v", r.CurrentSnapshot().Entries)
	}
}

func countES(s Snapshot) int {
	n := 0
	for _, e := range s.Entries {
		if e.Role == "ES" {
			n++
		}
	}
	return n
}
