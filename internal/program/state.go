// Package program maintains the program table view: PAT/PMT version
// change events and the PID mapping history.
package program

import (
	"fmt"

	"tsprobe/internal/psi"
)

// PIDMapEntry is one record of the PID mapping history.
type PIDMapEntry struct {
	Program    uint16 `json:"program"`
	PMTPID     uint16 `json:"pmt_pid"`
	StreamType uint8  `json:"stream_type"`
	PID        uint16 `json:"pid"`
	Packet     int64  `json:"packet"`
	Note       string `json:"note"` // new | type-changed | pmt-moved
}

// VersionEvent records one PAT or PMT version change.
type VersionEvent struct {
	Kind    string `json:"kind"` // PAT | PMT
	Program uint16 `json:"program,omitempty"`
	Old     int    `json:"old"`
	New     int    `json:"new"`
	Packet  int64  `json:"packet"`
}

type streamRef struct {
	typ    uint8
	pmtPID uint16
}

// State is the shared program table view. FeedPAT/FeedPMT must be
// called with CRC-valid sections in packet order.
type State struct {
	PATVersion int
	PATChanges int

	PMTVersion map[uint16]int
	PMTChanges map[uint16]int

	PMTPID   map[uint16]uint16 // program -> PMT PID
	PMTOwner map[uint16]uint16 // PMT PID -> program
	PCRPID   map[uint16]uint16
	Streams  map[uint16][]psi.Stream

	PIDMap []PIDMapEntry
	Events []VersionEvent

	// Revision increments on every applied table update (first sight
	// or version change); observers use it to detect view changes.
	Revision int64

	CurrentPAT []byte
	CurrentPMT map[uint16][]byte // program -> raw section
	SeenPMT    map[uint16]bool   // PMT PID -> valid section received

	seenPAT bool
	cur     map[uint16]map[uint16]streamRef // program -> pid -> ref
}

func NewState() *State {
	return &State{
		PATVersion: -1,
		PMTVersion: map[uint16]int{},
		PMTChanges: map[uint16]int{},
		PMTPID:     map[uint16]uint16{},
		PMTOwner:   map[uint16]uint16{},
		PCRPID:     map[uint16]uint16{},
		Streams:    map[uint16][]psi.Stream{},
		CurrentPMT: map[uint16][]byte{},
		SeenPMT:    map[uint16]bool{},
		cur:        map[uint16]map[uint16]streamRef{},
	}
}

// FeedPAT consumes one PAT section. A repeated version is not counted;
// any different version (including 31 -> 0 wrap) counts as one change.
func (s *State) FeedPAT(sec []byte, pkt int64) error {
	pat, err := psi.ParsePAT(sec)
	if err != nil {
		return err
	}
	if s.seenPAT && pat.Version == s.PATVersion {
		return nil
	}
	if s.seenPAT {
		s.PATChanges++
		s.Events = append(s.Events, VersionEvent{
			Kind: "PAT", Old: s.PATVersion, New: pat.Version, Packet: pkt,
		})
	}
	s.seenPAT = true
	s.PATVersion = pat.Version
	s.CurrentPAT = append(s.CurrentPAT[:0], sec...)
	s.Revision++
	for _, e := range pat.Entries {
		if old, ok := s.PMTPID[e.Program]; ok && old == e.PMTPID {
			continue
		}
		s.PMTPID[e.Program] = e.PMTPID
		s.PMTOwner[e.PMTPID] = e.Program
	}
	return nil
}

// FeedPMT consumes one PMT section received on pmtPID.
func (s *State) FeedPMT(pmtPID uint16, sec []byte, pkt int64) error {
	pmt, err := psi.ParsePMT(sec)
	if err != nil {
		return err
	}
	prog := pmt.Program
	s.SeenPMT[pmtPID] = true
	s.CurrentPMT[prog] = append(s.CurrentPMT[prog][:0], sec...)
	if v, ok := s.PMTVersion[prog]; ok && v == pmt.Version {
		return nil
	}
	if old, ok := s.PMTVersion[prog]; ok {
		s.PMTChanges[prog]++
		s.Events = append(s.Events, VersionEvent{
			Kind: "PMT", Program: prog, Old: old, New: pmt.Version, Packet: pkt,
		})
	}
	s.PMTVersion[prog] = pmt.Version
	s.PCRPID[prog] = pmt.PCRPID
	s.Streams[prog] = pmt.Streams
	s.Revision++
	if s.cur[prog] == nil {
		s.cur[prog] = map[uint16]streamRef{}
	}
	for _, st := range pmt.Streams {
		prev, ok := s.cur[prog][st.PID]
		note := ""
		switch {
		case !ok:
			note = "new"
		case prev.typ != st.Type:
			note = "type-changed"
		case prev.pmtPID != pmtPID:
			note = "pmt-moved"
		}
		if note != "" {
			s.PIDMap = append(s.PIDMap, PIDMapEntry{
				Program: prog, PMTPID: pmtPID, StreamType: st.Type,
				PID: st.PID, Packet: pkt, Note: note,
			})
			s.cur[prog][st.PID] = streamRef{typ: st.Type, pmtPID: pmtPID}
		}
	}
	return nil
}

// Validate reports unrecoverable table problems: PAT never seen, or a
// PMT PID announced by the PAT that never delivered a valid section.
func (s *State) Validate() []string {
	var problems []string
	if !s.seenPAT {
		problems = append(problems, "PAT missing: no valid PAT section received")
	}
	for prog, pid := range s.PMTPID {
		if !s.SeenPMT[pid] {
			problems = append(problems,
				fmt.Sprintf("program %d: PMT PID 0x%04X never received", prog, pid))
		}
	}
	return problems
}
