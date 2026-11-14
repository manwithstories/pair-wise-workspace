// Package model maintains versioned PSI tables and PID snapshots.
package model

import (
	"sort"

	"tsaudit/internal/psi"
)

// VersionInfo records the first packet carrying a table version.
type VersionInfo struct {
	Version     uint8 `json:"version"`
	FirstPacket int64 `json:"first_pkt"`
	CurrentNext bool  `json:"current_next"`
}

// PATTracker keeps PAT versions and the currently applicable program map.
type PATTracker struct {
	TransportStreamID uint16
	CurrentVersion    uint8
	CurrentNext       bool
	VersionChanges    int
	Versions          []VersionInfo
	Programs          []psi.PATProgram
	Current           *psi.PAT
	KnownPMTPIDs      map[uint16]bool
	firstPackets      map[uint8]int64
	lastApplied       uint8
	hasApplied        bool
}

func newPATTracker() *PATTracker {
	return &PATTracker{KnownPMTPIDs: make(map[uint16]bool), firstPackets: make(map[uint8]int64)}
}

// Ingest applies a PAT section. current_next=0 sections are recorded but never
// made active. Any applicable version transition, including 31->0, is counted.
func (t *PATTracker) Ingest(p *psi.PAT) {
	if p.TransportStreamID != 0 {
		t.TransportStreamID = p.TransportStreamID
	}
	if _, ok := t.firstPackets[p.Version]; !ok {
		t.firstPackets[p.Version] = p.StartPacket
		t.Versions = append(t.Versions, VersionInfo{Version: p.Version, FirstPacket: p.StartPacket, CurrentNext: p.CurrentNext})
	}
	for _, pr := range p.Programs {
		if !pr.IsNIT {
			t.KnownPMTPIDs[pr.PID] = true
		}
	}
	if !p.CurrentNext {
		return
	}
	if t.hasApplied && p.Version != t.lastApplied {
		t.VersionChanges++
	}
	t.lastApplied, t.hasApplied = p.Version, true
	t.CurrentVersion, t.CurrentNext = p.Version, p.CurrentNext
	t.Programs = append([]psi.PATProgram(nil), p.Programs...)
	t.Current = p
}

// PMTTracker keeps PMT versions and ES declarations for one program.
type PMTTracker struct {
	ProgramNumber  uint16
	CurrentVersion uint8
	CurrentNext    bool
	VersionChanges int
	Versions       []VersionInfo
	Current        *psi.PMT
	PMTPIDs        []uint16
	CurrentPMTPID  uint16
	firstPackets   map[uint8]int64
	lastApplied    uint8
	hasApplied     bool
	pidSeen        map[uint16]bool
}

func newPMTTracker(program uint16) *PMTTracker {
	return &PMTTracker{ProgramNumber: program, firstPackets: make(map[uint8]int64), pidSeen: make(map[uint16]bool)}
}

// Ingest applies a PMT section for this program.
func (t *PMTTracker) Ingest(pmtPID uint16, p *psi.PMT) {
	if !t.pidSeen[pmtPID] {
		t.pidSeen[pmtPID] = true
		t.PMTPIDs = append(t.PMTPIDs, pmtPID)
	}
	if _, ok := t.firstPackets[p.Version]; !ok {
		t.firstPackets[p.Version] = p.StartPacket
		t.Versions = append(t.Versions, VersionInfo{Version: p.Version, FirstPacket: p.StartPacket, CurrentNext: p.CurrentNext})
	}
	if !p.CurrentNext {
		return
	}
	if t.hasApplied && p.Version != t.lastApplied {
		t.VersionChanges++
	}
	t.lastApplied, t.hasApplied = p.Version, true
	t.CurrentVersion, t.CurrentNext = p.Version, p.CurrentNext
	t.Current = p
	t.CurrentPMTPID = pmtPID
}

// State is the live table cache and PID ledger for one transport stream.
type State struct {
	PAT      *PATTracker
	PMTs     map[uint16]*PMTTracker
	PIDStats map[uint16]*PIDStats
	NullPkts int64
	PMTPIDs  map[uint16]bool
	History  map[uint16]Declaration
}

// NewState creates an empty table/PID state.
func NewState() *State {
	return &State{
		PAT:      newPATTracker(),
		PMTs:     make(map[uint16]*PMTTracker),
		PIDStats: make(map[uint16]*PIDStats),
		PMTPIDs:  make(map[uint16]bool),
		History:  make(map[uint16]Declaration),
	}
}

// IngestPAT adds a PAT section to the version cache.
func (s *State) IngestPAT(p *psi.PAT) {
	s.PAT.Ingest(p)
	s.History[0] = Declaration{Kind: "PAT"}
	for _, pr := range p.Programs {
		if !pr.IsNIT {
			s.History[pr.PID] = Declaration{Kind: "PMT", PMTPID: pr.PID}
		}
	}
}

// IngestPMT adds a PMT section and records its PID.
func (s *State) IngestPMT(pid uint16, p *psi.PMT) {
	t := s.PMTs[p.ProgramNumber]
	if t == nil {
		t = newPMTTracker(p.ProgramNumber)
		s.PMTs[p.ProgramNumber] = t
	}
	t.Ingest(pid, p)
	s.PMTPIDs[pid] = true
	s.History[pid] = Declaration{Kind: "PMT", PMTPID: pid}
	if !p.CurrentNext {
		return
	}
	if p.PCRPID != 0x1fff {
		if _, ok := s.History[p.PCRPID]; !ok {
			s.History[p.PCRPID] = Declaration{Kind: "PCR", Program: p.ProgramNumber, PMTPID: pid}
		}
	}
	for _, es := range p.Streams {
		s.History[es.PID] = Declaration{
			Kind: "ES", Program: p.ProgramNumber, StreamType: es.StreamType,
			Language: es.Language, Registration: append([]string(nil), es.Registration...), PMTPID: pid,
		}
	}
}

// IsPMTPID reports whether a PID is known as a PMT PID, including old remaps.
func (s *State) IsPMTPID(pid uint16) bool { return s.PMTPIDs[pid] || s.PAT.KnownPMTPIDs[pid] }

// PMTTrackers returns program trackers in program-number order.
func (s *State) PMTTrackers() []*PMTTracker {
	out := make([]*PMTTracker, 0, len(s.PMTs))
	for _, t := range s.PMTs {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProgramNumber < out[j].ProgramNumber })
	return out
}

// ActiveDeclarations returns the current ES declarations keyed by PID. It also
// includes PCR-only and PMT control PIDs so they are not reported as orphans.
func (s *State) ActiveDeclarations() map[uint16]Declaration {
	out := make(map[uint16]Declaration)
	out[0] = Declaration{Kind: "PAT", PMTPID: 0, Language: ""}
	for pid := range s.PAT.KnownPMTPIDs {
		out[pid] = Declaration{Kind: "PMT", PMTPID: pid}
	}
	for _, t := range s.PMTs {
		if t.Current == nil {
			continue
		}
		if t.Current.PCRPID != 0x1fff {
			if _, exists := out[t.Current.PCRPID]; !exists {
				out[t.Current.PCRPID] = Declaration{Kind: "PCR", Program: t.ProgramNumber, PMTPID: t.CurrentPMTPID}
			}
		}
		for _, es := range t.Current.Streams {
			out[es.PID] = Declaration{
				Kind:         "ES",
				Program:      t.ProgramNumber,
				StreamType:   es.StreamType,
				Language:     es.Language,
				Registration: append([]string(nil), es.Registration...),
				PMTPID:       t.CurrentPMTPID,
			}
		}
	}
	return out
}

// Declarations returns every PID declaration seen in any applicable table. It
// distinguishes a remapped historical ES PID from a true orphan.
func (s *State) Declarations() map[uint16]Declaration {
	out := make(map[uint16]Declaration, len(s.History))
	for pid, d := range s.History {
		out[pid] = d
	}
	for pid, d := range s.ActiveDeclarations() {
		out[pid] = d
	}
	return out
}

// Declaration describes the latest active mapping for a PID.
type Declaration struct {
	Kind         string
	Program      uint16
	StreamType   uint8
	Language     string
	Registration []string
	PMTPID       uint16
}
