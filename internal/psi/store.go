package psi

import (
	"sort"
)

// StreamMap is one elementary-stream mapping in a table snapshot.
type StreamMap struct {
	PID        uint16 `json:"pid"`
	StreamType uint8  `json:"stream_type"`
}

// ProgramMap is a program-level table snapshot.
type ProgramMap struct {
	ProgramNumber uint16      `json:"program_number"`
	PMTPID        uint16      `json:"pmt_pid"`
	PCRPID        uint16      `json:"pcr_pid,omitempty"`
	Stale         bool        `json:"stale"`
	Streams       []StreamMap `json:"streams"`
}

// MappingSnapshot is the complete active program/elementary PID map.
type MappingSnapshot struct {
	Programs []ProgramMap `json:"programs"`
}

// MappingPoint records when the active mapping changed in the input.
type MappingPoint struct {
	PacketIndex uint64          `json:"packet_index"`
	ByteOffset  int64           `json:"byte_offset"`
	Snapshot    MappingSnapshot `json:"snapshot"`
}

// VersionEvent records one real version transition.
type VersionEvent struct {
	Table            string          `json:"table"`
	PID              uint16          `json:"pid"`
	ProgramNumber    uint16          `json:"program_number,omitempty"`
	PacketIndex      uint64          `json:"packet_index"`
	ByteOffset       int64           `json:"byte_offset"`
	PreviousVersion  uint8           `json:"previous_version"`
	NewVersion       uint8           `json:"new_version"`
	PreviousSnapshot MappingSnapshot `json:"previous_snapshot"`
	Snapshot         MappingSnapshot `json:"snapshot"`
}

// TableInfo is a stable summary of one PAT or PMT PID state.
type TableInfo struct {
	Table          string
	PID            uint16
	ProgramNumber  uint16
	VersionChanges int
	FirstVersion   uint8
	LastVersion    uint8
	HasVersion     bool
	Stale          bool
	Active         bool
	Mapping        MappingSnapshot
}

// StreamRef identifies the current owner of an elementary PID.
type StreamRef struct {
	PID           uint16
	ProgramNumber uint16
	StreamType    uint8
	PMTPID        uint16
}

type patState struct {
	has      bool
	version  uint8
	programs map[uint16]uint16
	stale    bool
	changes  int
	first    uint8
	last     uint8
}

type pmtState struct {
	has     bool
	info    PMT
	stale   bool
	changes int
	first   uint8
	last    uint8
}

// Store is the single shared PSI state machine used by scan and slice.
type Store struct {
	asm        *Assembler
	pat        patState
	pmts       map[uint16]*pmtState
	sdt        *SDT
	ignored    uint64
	unknown    uint64
	events     []VersionEvent
	mappings   []MappingPoint
	streamRefs map[uint16]StreamRef
}

// NewStore creates an empty PSI state machine.
func NewStore() *Store {
	return &Store{
		asm:        NewAssembler(),
		pmts:       make(map[uint16]*pmtState),
		streamRefs: make(map[uint16]StreamRef),
	}
}

// HandlePacket feeds one PSI PID payload through section assembly and table state.
func (s *Store) HandlePacket(pid uint16, payload []byte, pusi bool, packetIndex uint64, byteOffset int64) {
	sections, errs := s.asm.Push(pid, payload, pusi, packetIndex, byteOffset)
	for _, e := range errs {
		s.markStale(e.PID)
	}
	for _, sec := range sections {
		switch sec.TableID {
		case TableIDPAT:
			s.applyPAT(sec)
		case TableIDPMT:
			s.applyPMT(sec)
		case TableIDSDT:
			s.applySDT(sec)
		default:
			s.unknown++
		}
	}
}

// Finish closes any section left incomplete at end of input.
func (s *Store) Finish() {
	for _, e := range s.asm.Flush() {
		s.markStale(e.PID)
	}
}

func (s *Store) markStale(pid uint16) {
	if pid == 0 && s.pat.has {
		s.pat.stale = true
	}
	if st := s.pmts[pid]; st != nil && st.has {
		st.stale = true
	}
}

func (s *Store) applyPAT(sec Section) {
	p, err := ParsePAT(sec)
	if err != nil {
		return
	}
	if !p.CurrentNext {
		s.ignored++
		return
	}
	before := s.snapshot()
	changed := false
	fresh := false
	var previousVersion uint8
	if !s.pat.has {
		s.pat = patState{has: true, version: p.Version, programs: make(map[uint16]uint16), first: p.Version, last: p.Version}
		previousVersion = p.Version
		fresh = true
	} else if s.pat.version != p.Version {
		previousVersion = s.pat.version
		s.pat.changes++
		changed = true
		s.pat.programs = make(map[uint16]uint16)
	}
	for _, entry := range p.Programs {
		s.pat.programs[entry.ProgramNumber] = entry.PMTPID
	}
	s.pat.has = true
	s.pat.version = p.Version
	s.pat.stale = false
	s.pat.last = p.Version
	s.rebuildRefs()
	after := s.snapshot()
	if changed {
		s.events = append(s.events, VersionEvent{
			Table: "PAT", PID: sec.PID, PacketIndex: sec.PacketIndex, ByteOffset: sec.ByteOffset,
			PreviousVersion: previousVersion, NewVersion: p.Version,
			PreviousSnapshot: before, Snapshot: after,
		})
	}
	if changed || fresh {
		s.addMapping(sec, after)
	}
}

func (s *Store) applyPMT(sec Section) {
	p, err := ParsePMT(sec)
	if err != nil {
		return
	}
	if !p.CurrentNext {
		s.ignored++
		return
	}
	before := s.snapshot()
	st := s.pmts[sec.PID]
	if st == nil {
		st = &pmtState{}
		s.pmts[sec.PID] = st
	}
	previousVersion := st.info.Version
	changed := false
	fresh := false
	if !st.has {
		st.first = p.Version
		previousVersion = p.Version
		st.has = true
		fresh = true
	} else if st.info.Version != p.Version {
		st.changes++
		changed = true
	}
	st.info = *p
	st.last = p.Version
	st.stale = false
	s.rebuildRefs()
	after := s.snapshot()
	if changed {
		s.events = append(s.events, VersionEvent{
			Table: "PMT", PID: sec.PID, ProgramNumber: p.ProgramNumber,
			PacketIndex: sec.PacketIndex, ByteOffset: sec.ByteOffset,
			PreviousVersion: previousVersion, NewVersion: p.Version,
			PreviousSnapshot: before, Snapshot: after,
		})
	}
	if changed || fresh {
		s.addMapping(sec, after)
	}
}

func (s *Store) applySDT(sec Section) {
	p, err := ParseSDT(sec)
	if err != nil {
		return
	}
	if p.CurrentNext {
		s.sdt = p
	}
}

func (s *Store) addMapping(sec Section, snap MappingSnapshot) {
	s.mappings = append(s.mappings, MappingPoint{PacketIndex: sec.PacketIndex, ByteOffset: sec.ByteOffset, Snapshot: snap})
}

func (s *Store) snapshot() MappingSnapshot {
	var numbers []uint16
	for n := range s.pat.programs {
		numbers = append(numbers, n)
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	snap := MappingSnapshot{Programs: make([]ProgramMap, 0, len(numbers))}
	for _, n := range numbers {
		pmtPID := s.pat.programs[n]
		pm := ProgramMap{ProgramNumber: n, PMTPID: pmtPID, Streams: []StreamMap{}}
		if st := s.pmts[pmtPID]; st != nil && st.has {
			pm.PCRPID = st.info.PCRPID
			pm.Stale = st.stale
			for _, es := range st.info.Streams {
				pm.Streams = append(pm.Streams, StreamMap{PID: es.ElementaryPID, StreamType: es.StreamType})
			}
			sort.Slice(pm.Streams, func(i, j int) bool { return pm.Streams[i].PID < pm.Streams[j].PID })
		}
		snap.Programs = append(snap.Programs, pm)
	}
	return snap
}

func (s *Store) rebuildRefs() {
	s.streamRefs = make(map[uint16]StreamRef)
	for n, pmtPID := range s.pat.programs {
		st := s.pmts[pmtPID]
		if st == nil || !st.has {
			continue
		}
		for _, es := range st.info.Streams {
			if _, exists := s.streamRefs[es.ElementaryPID]; !exists {
				s.streamRefs[es.ElementaryPID] = StreamRef{PID: es.ElementaryPID, ProgramNumber: n, StreamType: es.StreamType, PMTPID: pmtPID}
			}
		}
	}
}

// Elementary returns the current mapping owner of an elementary PID.
func (s *Store) Elementary(pid uint16) (StreamRef, bool) {
	ref, ok := s.streamRefs[pid]
	return ref, ok
}

// PSIKind returns the table expected on a PSI PID, if any.
func (s *Store) PSIKind(pid uint16) (uint8, bool) {
	if pid == 0 {
		return TableIDPAT, true
	}
	if pid == 0x11 {
		return TableIDSDT, true
	}
	if _, ok := s.pmts[pid]; ok {
		return TableIDPMT, true
	}
	for _, pmtPID := range s.pat.programs {
		if pmtPID == pid {
			return TableIDPMT, true
		}
	}
	return 0, false
}

// Snapshot returns the current active mapping.
func (s *Store) Snapshot() MappingSnapshot { return s.snapshot() }

// MappingPoints returns the ordered mapping history.
func (s *Store) MappingPoints() []MappingPoint { return s.mappings }

// Events returns all real PAT/PMT version transitions.
func (s *Store) Events() []VersionEvent { return s.events }

// PATInfo returns the current PAT state summary.
func (s *Store) PATInfo() TableInfo {
	return TableInfo{
		Table: "PAT", PID: 0, VersionChanges: s.pat.changes,
		FirstVersion: s.pat.first, LastVersion: s.pat.last, HasVersion: s.pat.has,
		Stale: s.pat.stale, Active: s.pat.has, Mapping: s.snapshot(),
	}
}

// PMTInfos returns PMT states sorted by PID.
func (s *Store) PMTInfos() []TableInfo {
	var pids []uint16
	for pid := range s.pmts {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	infos := make([]TableInfo, 0, len(pids))
	for _, pid := range pids {
		st := s.pmts[pid]
		active := false
		for _, mapped := range s.pat.programs {
			if mapped == pid && s.pat.has {
				active = true
			}
		}
		infos = append(infos, TableInfo{
			Table: "PMT", PID: pid, ProgramNumber: st.info.ProgramNumber,
			VersionChanges: st.changes, FirstVersion: st.first, LastVersion: st.last,
			HasVersion: st.has, Stale: st.stale, Active: active, Mapping: s.snapshot(),
		})
	}
	return infos
}

// SDT returns the last current/valid SDT.
func (s *Store) SDT() *SDT { return s.sdt }

// CRCErrors returns rejected section count.
func (s *Store) CRCErrors() uint64 { return s.asm.CRCErrors() }

// IncompleteSections returns abandoned partial-section count.
func (s *Store) IncompleteSections() uint64 { return s.asm.Incomplete() }

// IgnoredNext returns current_next_indicator=0 sections not applied.
func (s *Store) IgnoredNext() uint64 { return s.ignored }

// UnknownSections returns unrecognised table_id count.
func (s *Store) UnknownSections() uint64 { return s.unknown }

// Stale reports whether any current active table was last seen corrupted.
func (s *Store) Stale() bool {
	if s.pat.stale {
		return true
	}
	for n, pid := range s.pat.programs {
		_ = n
		if st := s.pmts[pid]; st != nil && st.stale {
			return true
		}
	}
	return false
}
