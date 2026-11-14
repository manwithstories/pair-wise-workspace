package psi

import (
	"fmt"
	"sort"
)

type VersionEvent struct {
	Version  uint8
	Rollback bool
}

type PMTVersionEvent struct {
	Program uint16
	VersionEvent
}

// PIDInterval is one version-bounded mapping. EndVersion and PATEndVersion are
// inclusive source versions at which the mapping was last observed.
type PIDInterval struct {
	PID             uint16 `json:"pid"`
	Program         uint16 `json:"program"`
	StreamType      string `json:"stream_type"`
	StartVersion    uint8  `json:"start_version"`
	EndVersion      uint8  `json:"end_version"`
	PATStartVersion uint8  `json:"start_pat_version"`
	PATEndVersion   uint8  `json:"end_pat_version"`
	PacketCount     int64  `json:"packet_count"`

	streamTypeByte byte
	active         bool
}

type ProgramInterval struct {
	Program         uint16 `json:"program"`
	PMTPID          uint16 `json:"pmt_pid"`
	StartPATVersion uint8  `json:"start_pat_version"`
	EndPATVersion   uint8  `json:"end_pat_version"`
}

type programState struct {
	program uint16
	pmtPID  uint16
	start   uint8
	active  bool
}

type ProgramVersions struct {
	Program  uint16 `json:"program"`
	Versions int    `json:"versions"`
}

// Store is the authoritative PSI state machine shared by scan and slice.
type Store struct {
	pat            PAT
	patValid       bool
	currentPATVer  uint8
	lastPATVer     uint8
	lastPATValid   bool
	patSeen        map[uint8]bool
	patEvents      []VersionEvent
	patRollbacks   int
	programs       map[uint16]*programState
	programHistory []ProgramInterval
	pmts           map[uint16]PMT
	pmtSeen        map[uint16]map[uint8]bool
	pmtLastVersion map[uint16]uint8
	pmtLastValid   map[uint16]bool
	pmtRollbacks   map[uint16]int
	pmtEvents      map[uint16][]VersionEvent
	intervals      []*PIDInterval
	activeByPID    map[uint16]*PIDInterval
	knownPIDs      map[uint16]bool
	pmtPIDs        map[uint16]bool
	packetCounts   map[uint16]int64
}

func NewStore() *Store {
	return &Store{
		patSeen:        make(map[uint8]bool),
		programs:       make(map[uint16]*programState),
		pmts:           make(map[uint16]PMT),
		pmtSeen:        make(map[uint16]map[uint8]bool),
		pmtLastVersion: make(map[uint16]uint8),
		pmtLastValid:   make(map[uint16]bool),
		pmtRollbacks:   make(map[uint16]int),
		pmtEvents:      make(map[uint16][]VersionEvent),
		activeByPID:    make(map[uint16]*PIDInterval),
		knownPIDs:      make(map[uint16]bool),
		pmtPIDs:        make(map[uint16]bool),
		packetCounts:   make(map[uint16]int64),
	}
}

// Consume applies one CRC-valid completed section. Invalid CRC and malformed
// sections are rejected before touching any state.
func (s *Store) Consume(sec Section) error {
	if sec.Err != "" || !sec.CRCValid || len(sec.Data) < 3 {
		return fmt.Errorf("psi: refusing invalid section: %s", sec.Err)
	}
	switch sec.Data[0] {
	case TablePAT:
		pat, err := ParsePAT(sec.Data)
		if err != nil {
			return err
		}
		return s.consumePAT(pat)
	case TablePMT:
		pmt, err := ParsePMT(sec.Data)
		if err != nil {
			return err
		}
		return s.consumePMT(sec.PID, pmt)
	case TableSDT:
		_, err := ParseSDT(sec.Data)
		return err
	default:
		return nil
	}
}

func (s *Store) consumePAT(pat PAT) error {
	if !pat.CurrentNext {
		return nil
	}
	rollback := s.lastPATValid && pat.Version < s.lastPATVer
	s.patSeen[pat.Version] = true
	s.patEvents = append(s.patEvents, VersionEvent{Version: pat.Version, Rollback: rollback})
	if rollback {
		s.patRollbacks++
	}
	s.pat = pat
	s.patValid = true
	s.currentPATVer = pat.Version
	s.lastPATVer = pat.Version
	s.lastPATValid = true

	next := make(map[uint16]uint16)
	var networkPID uint16
	for _, entry := range pat.Programs {
		if entry.Program == 0 {
			networkPID = entry.PID
			continue
		}
		next[entry.Program] = entry.PID
	}
	_ = networkPID

	for program, state := range s.programs {
		newPID, ok := next[program]
		if !ok || newPID != state.pmtPID {
			s.closeProgram(program)
		}
	}
	for program, pmtPID := range next {
		if state := s.programs[program]; state != nil && state.active {
			continue
		}
		s.programs[program] = &programState{program: program, pmtPID: pmtPID, start: pat.Version, active: true}
		s.programHistory = append(s.programHistory, ProgramInterval{
			Program: program, PMTPID: pmtPID, StartPATVersion: pat.Version, EndPATVersion: pat.Version,
		})
	}
	for _, interval := range s.intervals {
		if interval.active {
			interval.PATEndVersion = pat.Version
		}
	}
	for i := range s.programHistory {
		history := &s.programHistory[i]
		if state := s.programs[history.Program]; state != nil && state.active && state.pmtPID == history.PMTPID {
			history.EndPATVersion = pat.Version
		}
	}
	s.rebuildKnownPIDs()
	return nil
}

func (s *Store) closeProgram(program uint16) {
	state := s.programs[program]
	if state == nil {
		return
	}
	state.active = false
	for i := len(s.programHistory) - 1; i >= 0; i-- {
		h := &s.programHistory[i]
		if h.Program == program && h.PMTPID == state.pmtPID {
			h.EndPATVersion = s.currentPATVer
			break
		}
	}
	for pid, interval := range s.activeByPID {
		if interval.Program == program {
			s.closeInterval(pid, interval)
		}
	}
	delete(s.pmts, program)
	state.pmtPID = 0
}

func (s *Store) closeInterval(pid uint16, interval *PIDInterval) {
	interval.active = false
	interval.PATEndVersion = s.currentPATVer
	if s.activeByPID[pid] == interval {
		delete(s.activeByPID, pid)
	}
}

func (s *Store) rebuildKnownPIDs() {
	s.knownPIDs = make(map[uint16]bool)
	s.pmtPIDs = make(map[uint16]bool)
	for _, state := range s.programs {
		if state.active && state.pmtPID != 0 {
			s.pmtPIDs[state.pmtPID] = true
			s.knownPIDs[state.pmtPID] = true
		}
	}
	for _, interval := range s.activeByPID {
		s.knownPIDs[interval.PID] = true
	}
}

func (s *Store) HasPAT() bool { return s.patValid }

func (s *Store) IsPSIPID(pid uint16) bool {
	return pid == TablePAT || pid == 0x11 || s.pmtPIDs[pid]
}

func (s *Store) IsKnownPID(pid uint16) bool { return s.knownPIDs[pid] }

func (s *Store) LookupPID(pid uint16) (program uint16, streamType byte, ok bool) {
	interval := s.activeByPID[pid]
	if interval == nil || !interval.active {
		return 0, 0, false
	}
	return interval.Program, interval.streamTypeByte, true
}

func (s *Store) RecordPacket(pid uint16) bool {
	if !s.knownPIDs[pid] {
		return false
	}
	s.packetCounts[pid]++
	if interval := s.activeByPID[pid]; interval != nil && interval.active {
		interval.PacketCount++
	}
	return true
}

func (s *Store) PATVersions() int { return len(s.patSeen) }

func (s *Store) PMTVersions() []ProgramVersions {
	programs := make([]uint16, 0, len(s.pmtSeen))
	for program := range s.pmtSeen {
		programs = append(programs, program)
	}
	sort.Slice(programs, func(i, j int) bool { return programs[i] < programs[j] })
	out := make([]ProgramVersions, 0, len(programs))
	for _, program := range programs {
		out = append(out, ProgramVersions{Program: program, Versions: len(s.pmtSeen[program])})
	}
	return out
}

func (s *Store) CurrentPMTVersions() []ProgramVersions {
	programs := make([]uint16, 0, len(s.pmts))
	for program := range s.pmts {
		programs = append(programs, program)
	}
	sort.Slice(programs, func(i, j int) bool { return programs[i] < programs[j] })
	out := make([]ProgramVersions, 0, len(programs))
	for _, program := range programs {
		out = append(out, ProgramVersions{Program: program, Versions: int(s.pmts[program].Version)})
	}
	return out
}

func (s *Store) PacketCounts() map[uint16]int64 {
	out := make(map[uint16]int64, len(s.packetCounts))
	for pid, count := range s.packetCounts {
		out[pid] = count
	}
	return out
}

func (s *Store) PATRollbacks() int { return s.patRollbacks }

func (s *Store) PMTRollbacks() map[uint16]int {
	out := make(map[uint16]int, len(s.pmtRollbacks))
	for program, count := range s.pmtRollbacks {
		out[program] = count
	}
	return out
}

func (s *Store) consumePMT(pid uint16, pmt PMT) error {
	if !pmt.CurrentNext {
		return nil
	}
	state := s.programs[pmt.Program]
	if state == nil || !state.active || state.pmtPID != pid {
		return nil
	}
	current := make(map[uint16]byte, len(pmt.Streams))
	for _, stream := range pmt.Streams {
		if _, duplicate := current[stream.ElementaryPID]; duplicate {
			return fmt.Errorf("psi: PMT program %d repeats PID %d", pmt.Program, stream.ElementaryPID)
		}
		current[stream.ElementaryPID] = stream.StreamType
	}
	seen := s.pmtSeen[pmt.Program]
	if seen == nil {
		seen = make(map[uint8]bool)
		s.pmtSeen[pmt.Program] = seen
	}
	rollback := s.pmtLastValid[pmt.Program] && pmt.Version < s.pmtLastVersion[pmt.Program]
	seen[pmt.Version] = true
	s.pmtEvents[pmt.Program] = append(s.pmtEvents[pmt.Program],
		VersionEvent{Version: pmt.Version, Rollback: rollback})
	if rollback {
		s.pmtRollbacks[pmt.Program]++
	}
	s.pmtLastVersion[pmt.Program] = pmt.Version
	s.pmtLastValid[pmt.Program] = true
	s.pmts[pmt.Program] = pmt

	for streamPID, interval := range s.activeByPID {
		if interval.Program != pmt.Program {
			continue
		}
		streamType, present := current[streamPID]
		if !present || streamType != interval.streamTypeByte {
			s.closeInterval(streamPID, interval)
		}
	}
	for streamPID, streamType := range current {
		if interval := s.activeByPID[streamPID]; interval != nil {
			if interval.Program == pmt.Program && interval.streamTypeByte == streamType {
				interval.EndVersion = pmt.Version
				interval.PATEndVersion = s.currentPATVer
				continue
			}
			s.closeInterval(streamPID, interval)
		}
		interval := &PIDInterval{
			PID:             streamPID,
			Program:         pmt.Program,
			StreamType:      StreamTypeName(streamType),
			StartVersion:    pmt.Version,
			EndVersion:      pmt.Version,
			PATStartVersion: s.currentPATVer,
			PATEndVersion:   s.currentPATVer,
			streamTypeByte:  streamType,
			active:          true,
		}
		s.intervals = append(s.intervals, interval)
		s.activeByPID[streamPID] = interval
	}
	s.rebuildKnownPIDs()
	return nil
}

func (s *Store) PIDMap() []PIDInterval {
	out := make([]PIDInterval, 0, len(s.intervals))
	for _, interval := range s.intervals {
		copy := *interval
		if copy.active {
			copy.PATEndVersion = s.currentPATVer
		}
		copy.active = false
		out = append(out, copy)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PID != out[j].PID {
			return out[i].PID < out[j].PID
		}
		if out[i].StartVersion != out[j].StartVersion {
			return out[i].StartVersion < out[j].StartVersion
		}
		return out[i].PATStartVersion < out[j].PATStartVersion
	})
	return out
}

// Snapshot is an immutable view used to seed each independently decodable clip.
type ProgramSnapshot struct {
	Program    uint16
	PMTPID     uint16
	PMTVersion uint8
	PMT        PMT
	HasPMT     bool
}

type Snapshot struct {
	TransportStreamID uint16
	PATVersion        uint8
	Programs          []ProgramSnapshot
}

func (s *Store) Snapshot() Snapshot {
	out := Snapshot{TransportStreamID: s.pat.TransportStreamID, PATVersion: s.currentPATVer}
	programs := make([]uint16, 0, len(s.programs))
	for program, state := range s.programs {
		if state.active {
			programs = append(programs, program)
		}
	}
	sort.Slice(programs, func(i, j int) bool { return programs[i] < programs[j] })
	for _, program := range programs {
		state := s.programs[program]
		item := ProgramSnapshot{Program: program, PMTPID: state.pmtPID}
		if pmt, ok := s.pmts[program]; ok {
			item.PMT = pmt
			item.PMTVersion = pmt.Version
			item.HasPMT = true
		}
		out.Programs = append(out.Programs, item)
	}
	return out
}

func (s *Store) DanglingPrograms() []uint16 {
	var out []uint16
	for program, state := range s.programs {
		if state.active {
			if _, ok := s.pmts[program]; !ok {
				out = append(out, program)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *Store) ProgramIntervals() []ProgramInterval {
	return append([]ProgramInterval(nil), s.programHistory...)
}

func (s *Store) PATVersionEvents() []VersionEvent {
	return append([]VersionEvent(nil), s.patEvents...)
}

func (s *Store) PMTVersionEvents() []PMTVersionEvent {
	var out []PMTVersionEvent
	for program, events := range s.pmtEvents {
		for _, event := range events {
			out = append(out, PMTVersionEvent{Program: program, VersionEvent: event})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Program != out[j].Program {
			return out[i].Program < out[j].Program
		}
		return out[i].Version < out[j].Version
	})
	return out
}
