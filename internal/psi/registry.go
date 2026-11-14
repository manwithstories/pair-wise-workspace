package psi

import "sort"

// VersionStats counts version transitions for one table.
type VersionStats struct {
	Changes    uint64 `json:"changes"`
	Repeats    uint64 `json:"repeats"`
	Rollbacks  uint64 `json:"rollbacks"`
	Skips      uint64 `json:"skips"`
	NotCurrent uint64 `json:"not_current"`
}

// tableState is the per-table version state machine.
type tableState struct {
	hasCurrent bool
	version    byte
	Stats      VersionStats
}

// observe updates the state machine with one received section version.
func (s *tableState) observe(version byte, currentNext bool) (changed bool) {
	if !currentNext {
		s.Stats.NotCurrent++
		return false
	}
	if !s.hasCurrent {
		s.hasCurrent = true
		s.version = version
		return true
	}
	diff := (version - s.version) & 0x1F
	switch {
	case diff == 0:
		s.Stats.Repeats++
		return false
	case diff == 1:
		s.Stats.Changes++
	case diff <= 16:
		s.Stats.Changes++
		s.Stats.Skips++
	default:
		s.Stats.Changes++
		s.Stats.Rollbacks++
	}
	s.version = version
	return true
}

// ProgramInfo is the registry's current view of one program.
type ProgramInfo struct {
	ProgramNumber uint16
	PMTPID        uint16
	PMT           *PMT
	Stats         VersionStats
}

// Registry tracks current PAT/PMT versions and per-table statistics.
type Registry struct {
	patState  tableState
	patParts  map[byte][]byte
	patLast   byte
	PAT       *PAT
	PATStats  VersionStats
	pmtState  map[uint16]*tableState
	pmtRaw    map[uint16][]byte
	pmtByPID  map[uint16]uint16
	PMTByProg map[uint16]*PMT
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		patParts:  map[byte][]byte{},
		pmtState:  map[uint16]*tableState{},
		pmtRaw:    map[uint16][]byte{},
		pmtByPID:  map[uint16]uint16{},
		PMTByProg: map[uint16]*PMT{},
	}
}

// ApplyPATSection feeds one raw PAT section into the registry.
func (r *Registry) ApplyPATSection(raw []byte) {
	pat, sec, last, ok := ParsePATSection(raw)
	if !ok {
		return
	}
	changed := r.patState.observe(pat.Version, pat.CurrentNext)
	r.PATStats = r.patState.Stats
	if !pat.CurrentNext {
		return
	}
	if changed || r.PAT == nil {
		r.patParts = map[byte][]byte{}
		r.patLast = last
	}
	r.patParts[sec] = append([]byte(nil), raw...)
	if last != r.patLast {
		r.patLast = last
	}
	r.mergePAT()
}

// mergePAT merges all sections 0..last when complete.
func (r *Registry) mergePAT() {
	if int(r.patLast)+1 != len(r.patParts) {
		return
	}
	merged := &PAT{Programs: map[uint16]uint16{}}
	for i := byte(0); i <= r.patLast; i++ {
		p, _, _, ok := ParsePATSection(r.patParts[i])
		if !ok {
			return
		}
		merged.TSID = p.TSID
		merged.Version = p.Version
		merged.CurrentNext = p.CurrentNext
		merged.NITPIDs = append(merged.NITPIDs, p.NITPIDs...)
		for prog, pid := range p.Programs {
			merged.Programs[prog] = pid
		}
	}
	r.PAT = merged
	for prog, pid := range merged.Programs {
		r.pmtByPID[pid] = prog
	}
}

// ApplyPMTSection feeds one raw PMT section (received on a PMT PID).
func (r *Registry) ApplyPMTSection(raw []byte) {
	pmt, ok := ParsePMT(raw)
	if !ok {
		return
	}
	st := r.pmtState[pmt.ProgramNumber]
	if st == nil {
		st = &tableState{}
		r.pmtState[pmt.ProgramNumber] = st
	}
	changed := st.observe(pmt.Version, pmt.CurrentNext)
	if pmt.CurrentNext && (changed || r.PMTByProg[pmt.ProgramNumber] == nil) {
		r.PMTByProg[pmt.ProgramNumber] = pmt
		r.pmtRaw[pmt.ProgramNumber] = append([]byte(nil), raw...)
	}
}

// IsPMTPID reports whether pid is a registered PMT PID.
func (r *Registry) IsPMTPID(pid uint16) bool {
	_, ok := r.pmtByPID[pid]
	return ok
}

// ProgramForPMTPID returns the program number for a PMT PID.
func (r *Registry) ProgramForPMTPID(pid uint16) (uint16, bool) {
	p, ok := r.pmtByPID[pid]
	return p, ok
}

// PATRawSections returns the raw sections of the current PAT, in order.
func (r *Registry) PATRawSections() [][]byte {
	var out [][]byte
	for i := byte(0); i <= r.patLast; i++ {
		if s, ok := r.patParts[i]; ok {
			out = append(out, s)
		}
	}
	return out
}

// PMTRawSections returns the raw current PMT section per program, sorted.
func (r *Registry) PMTRawSections() [][]byte {
	progs := make([]int, 0, len(r.pmtRaw))
	for p := range r.pmtRaw {
		progs = append(progs, int(p))
	}
	sort.Ints(progs)
	var out [][]byte
	for _, p := range progs {
		out = append(out, r.pmtRaw[uint16(p)])
	}
	return out
}

// Programs returns current program infos sorted by program number.
func (r *Registry) Programs() []ProgramInfo {
	var out []ProgramInfo
	if r.PAT == nil {
		return out
	}
	progs := make([]int, 0, len(r.PAT.Programs))
	for p := range r.PAT.Programs {
		progs = append(progs, int(p))
	}
	sort.Ints(progs)
	for _, p := range progs {
		prog := uint16(p)
		info := ProgramInfo{ProgramNumber: prog, PMTPID: r.PAT.Programs[prog]}
		info.PMT = r.PMTByProg[prog]
		if st := r.pmtState[prog]; st != nil {
			info.Stats = st.Stats
		}
		out = append(out, info)
	}
	return out
}
