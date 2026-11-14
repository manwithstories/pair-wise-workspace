package psi

import (
	"fmt"
	"sort"
)

type PIDEntry struct {
	PID         uint16       `json:"pid"`
	Program     uint16       `json:"program,omitempty"`
	Role        string       `json:"role"`
	StreamType  *uint8       `json:"stream_type,omitempty"`
	Descriptors []Descriptor `json:"descriptors,omitempty"`
	PacketCount int64        `json:"packet_count,omitempty"`
}

type Snapshot struct {
	ID          int               `json:"id"`
	Offset      int64             `json:"offset"`
	PATVersion  uint8             `json:"pat_version"`
	PMTVersions map[uint16]uint8  `json:"pmt_versions"`
	Programs    map[uint16]uint16 `json:"programs"`
	Entries     []PIDEntry        `json:"pid_map"`
}

type Conflict struct {
	SnapshotID int    `json:"snapshot_id"`
	PID        uint16 `json:"pid"`
	Program    uint16 `json:"program,omitempty"`
	Message    string `json:"message"`
}

func (r *Registry) CurrentSnapshot() Snapshot {
	return r.currentSnapshot
}

func (r *Registry) buildSnapshot(id int, offset int64) Snapshot {
	s := Snapshot{
		ID: id, Offset: offset, PMTVersions: make(map[uint16]uint8),
		Programs: make(map[uint16]uint16), Entries: []PIDEntry{},
	}
	if r.activePAT == nil {
		return s
	}
	s.PATVersion = r.activePAT.Version
	s.Entries = append(s.Entries, PIDEntry{PID: 0, Role: "PAT"})
	programs := make([]int, 0, len(r.activePAT.Programs))
	for program := range r.activePAT.Programs {
		programs = append(programs, int(program))
		s.Programs[program] = r.activePAT.Programs[program]
	}
	sort.Ints(programs)
	for _, rawProgram := range programs {
		program := uint16(rawProgram)
		pmtPID := r.activePAT.Programs[program]
		s.Entries = append(s.Entries, PIDEntry{PID: pmtPID, Program: program, Role: "PMT"})
		pmt := r.activePMTs[program]
		if pmt == nil {
			continue
		}
		s.PMTVersions[program] = pmt.Version
		for _, es := range pmt.Streams {
			streamType := es.StreamType
			s.Entries = append(s.Entries, PIDEntry{
				PID: es.PID, Program: program, Role: "ES", StreamType: &streamType,
				Descriptors: es.Descriptors,
			})
		}
	}
	return s
}

func (r *Registry) checkPSIOverlap(snapshotID int) {
	if r.activePAT == nil {
		return
	}
	psiPIDs := map[uint16]string{0: "PAT"}
	for program, pid := range r.activePAT.Programs {
		if _, exists := psiPIDs[pid]; exists {
			r.Conflicts = append(r.Conflicts, Conflict{
				SnapshotID: snapshotID, PID: pid, Program: program,
				Message: fmt.Sprintf("PMT PID 0x%04x is shared by multiple PSI roles", pid),
			})
		}
		psiPIDs[pid] = fmt.Sprintf("PMT/program %d", program)
	}
	for program, pmt := range r.activePMTs {
		if _, announced := r.activePAT.Programs[program]; !announced {
			continue
		}
		for _, es := range pmt.Streams {
			if role, exists := psiPIDs[es.PID]; exists {
				r.Conflicts = append(r.Conflicts, Conflict{
					SnapshotID: snapshotID, PID: es.PID, Program: program,
					Message: fmt.Sprintf("elementary PID overlaps %s", role),
				})
			}
		}
	}
}
