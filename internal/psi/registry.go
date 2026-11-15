package psi

import (
	"fmt"
	"sort"

	"github.com/asticode/go-astits"
)

type Assignment struct {
	ProgramNumber uint16
	PMTPID        uint16
	PMTVersion    uint8
	PMTRevision   int
	Effective     int64
	PID           uint16
	StreamType    uint8
	PCRPID        uint16
}

type Event struct {
	EffectivePacket int64
	Table           string
	ProgramNumber   uint16
	PMTPID          uint16
	Version         uint8
	Revision        int
	PMTPIDs         []uint16
	PIDs            []uint16
}

type PATSnapshot struct {
	Revision          int
	Version           uint8
	EffectivePacket   int64
	TransportStreamID uint16
	Programs          []PATProgram
}

type PMTSnapshot struct {
	Revision        int
	Version         uint8
	EffectivePacket int64
	PID             uint16
	ProgramNumber   uint16
	PCRPID          uint16
	Streams         []Stream
}

type ProgramState struct {
	Number  uint16
	PMTPID  uint16
	Current *PMTSnapshot
}

type ProgramContext struct {
	ProgramNumber uint16
	PMTPID        uint16
	PMTRevision   int
	PMTVersion    uint8
	PCRPID        uint16
	Streams       []Stream
}

type Context struct {
	PATRevision int
	PATVersion  uint8
	Epoch       int
	Programs    []ProgramContext
}

type Registry struct {
	pat         *PATSnapshot
	patHistory  []PATSnapshot
	programs    map[uint16]*ProgramState
	pmtHistory  map[uint16][]PMTSnapshot
	assignments map[uint16]Assignment
	pmtPIDs     map[uint16]bool
	events      []Event
	epoch       int
}

func NewRegistry() *Registry {
	return &Registry{
		programs:    make(map[uint16]*ProgramState),
		pmtHistory:  make(map[uint16][]PMTSnapshot),
		assignments: make(map[uint16]Assignment),
		pmtPIDs:     make(map[uint16]bool),
	}
}

func (r *Registry) Apply(c Change) ([]Event, error) {
	if c.Kind == TablePAT {
		return r.applyPAT(c)
	}
	if c.Kind == TablePMT {
		return r.applyPMT(c)
	}
	return nil, fmt.Errorf("psi: unknown table change %q", c.Kind)
}

func (r *Registry) applyPAT(c Change) ([]Event, error) {
	if c.PAT == nil {
		return nil, fmt.Errorf("psi: PAT change has no PAT")
	}
	if c.PacketIndex < 0 {
		return nil, fmt.Errorf("psi: PAT effective packet is negative")
	}
	revision := len(r.patHistory) + 1
	snapshot := PATSnapshot{
		Revision:          revision,
		Version:           c.PAT.Version,
		EffectivePacket:   c.PacketIndex,
		TransportStreamID: c.PAT.TransportStreamID,
		Programs:          append([]PATProgram(nil), c.PAT.Programs...),
	}
	r.pat = &snapshot
	r.patHistory = append(r.patHistory, snapshot)
	r.epoch++

	active := make(map[uint16]bool)
	var pmtPIDs []uint16
	for _, entry := range snapshot.Programs {
		active[entry.ProgramNumber] = true
		pmtPIDs = append(pmtPIDs, entry.PMTPID)
		state := r.programs[entry.ProgramNumber]
		if state == nil {
			state = &ProgramState{Number: entry.ProgramNumber}
			r.programs[entry.ProgramNumber] = state
		}
		if state.PMTPID != entry.PMTPID {
			state.PMTPID = entry.PMTPID
			state.Current = nil
		}
		r.pmtPIDs[entry.PMTPID] = true
	}
	for number, state := range r.programs {
		if !active[number] {
			state.Current = nil
		}
	}
	r.rebuildAssignments()
	event := Event{
		EffectivePacket: c.PacketIndex,
		Table:           TablePAT,
		Version:         c.PAT.Version,
		Revision:        revision,
		PMTPIDs:         sortedPIDSet(pmtPIDs),
		PIDs:            r.currentPIDSet(),
	}
	r.events = append(r.events, event)
	return []Event{event}, nil
}

func (r *Registry) applyPMT(c Change) ([]Event, error) {
	if c.PMT == nil {
		return nil, fmt.Errorf("psi: PMT change has no PMT")
	}
	state := r.programs[c.PMT.ProgramNumber]
	if state == nil || state.PMTPID != c.PID {
		return nil, nil
	}
	revision := len(r.pmtHistory[c.PMT.ProgramNumber]) + 1
	snapshot := PMTSnapshot{
		Revision:        revision,
		Version:         c.PMT.Version,
		EffectivePacket: c.PacketIndex,
		PID:             c.PID,
		ProgramNumber:   c.PMT.ProgramNumber,
		PCRPID:          c.PMT.PCRPID,
		Streams:         append([]Stream(nil), c.PMT.Streams...),
	}
	sort.Slice(snapshot.Streams, func(i, j int) bool { return snapshot.Streams[i].PID < snapshot.Streams[j].PID })
	r.pmtHistory[c.PMT.ProgramNumber] = append(r.pmtHistory[c.PMT.ProgramNumber], snapshot)
	r.pmtPIDs[c.PID] = true
	state.Current = &snapshot
	r.epoch++
	r.rebuildAssignments()
	event := Event{
		EffectivePacket: c.PacketIndex,
		Table:           TablePMT,
		ProgramNumber:   c.PMT.ProgramNumber,
		PMTPID:          c.PID,
		Version:         c.PMT.Version,
		Revision:        revision,
		PMTPIDs:         []uint16{c.PID},
		PIDs:            r.programPIDSet(c.PMT.ProgramNumber),
	}
	r.events = append(r.events, event)
	return []Event{event}, nil
}

func (r *Registry) rebuildAssignments() {
	r.assignments = make(map[uint16]Assignment)
	for _, entry := range r.currentPATPrograms() {
		state := r.programs[entry.ProgramNumber]
		if state == nil || state.Current == nil || state.Current.PID != entry.PMTPID {
			continue
		}
		for _, stream := range state.Current.Streams {
			r.assignments[stream.PID] = Assignment{
				ProgramNumber: entry.ProgramNumber,
				PMTPID:        entry.PMTPID,
				PMTVersion:    state.Current.Version,
				PMTRevision:   state.Current.Revision,
				Effective:     state.Current.EffectivePacket,
				PID:           stream.PID,
				StreamType:    stream.StreamType,
				PCRPID:        state.Current.PCRPID,
			}
		}
	}
}

func (r *Registry) LookupPID(pid uint16) (Assignment, bool) {
	a, ok := r.assignments[pid]
	return a, ok
}

func (r *Registry) IsPSIPID(pid uint16) bool {
	return pid == astits.PIDPAT || r.pmtPIDs[pid]
}

func (r *Registry) PAT() *PATSnapshot {
	if r.pat == nil {
		return nil
	}
	copy := *r.pat
	copy.Programs = append([]PATProgram(nil), r.pat.Programs...)
	return &copy
}

func (r *Registry) PATRevisions() int { return len(r.patHistory) }

func (r *Registry) PATHistory() []PATSnapshot {
	out := make([]PATSnapshot, len(r.patHistory))
	for i, snapshot := range r.patHistory {
		out[i] = snapshot
		out[i].Programs = append([]PATProgram(nil), snapshot.Programs...)
	}
	return out
}

func (r *Registry) PMTRevisions(program uint16) int { return len(r.pmtHistory[program]) }

func (r *Registry) PMTHistory(program uint16) []PMTSnapshot {
	history := r.pmtHistory[program]
	out := make([]PMTSnapshot, len(history))
	for i, snapshot := range history {
		out[i] = snapshot
		out[i].Streams = append([]Stream(nil), snapshot.Streams...)
	}
	return out
}

func (r *Registry) CurrentProgramNumbers() []uint16 {
	entries := r.currentPATPrograms()
	out := make([]uint16, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.ProgramNumber)
	}
	return out
}

func (r *Registry) CurrentPMTPID(program uint16) (uint16, bool) {
	for _, entry := range r.currentPATPrograms() {
		if entry.ProgramNumber == program {
			return entry.PMTPID, true
		}
	}
	return 0, false
}

func (r *Registry) RevisionEpoch() int { return r.epoch }

func (r *Registry) Events() []Event {
	out := make([]Event, len(r.events))
	for i, event := range r.events {
		out[i] = event
		out[i].PMTPIDs = clonePIDs(event.PMTPIDs)
		out[i].PIDs = clonePIDs(event.PIDs)
	}
	return out
}

func (r *Registry) EventsBetween(start, end int64) []Event {
	var out []Event
	for _, event := range r.events {
		if event.EffectivePacket > start && event.EffectivePacket <= end {
			copy := event
			copy.PMTPIDs = clonePIDs(event.PMTPIDs)
			copy.PIDs = clonePIDs(event.PIDs)
			out = append(out, copy)
		}
	}
	return out
}

func (r *Registry) ContextAt(packet int64) Context {
	var ctx Context
	var pat *PATSnapshot
	for _, snapshot := range r.patHistory {
		if snapshot.EffectivePacket > packet {
			break
		}
		copy := snapshot
		pat = &copy
	}
	if pat == nil {
		return ctx
	}
	ctx.PATRevision = pat.Revision
	ctx.PATVersion = pat.Version
	for _, event := range r.events {
		if event.EffectivePacket <= packet {
			ctx.Epoch++
		}
	}
	for _, entry := range pat.Programs {
		program := ProgramContext{ProgramNumber: entry.ProgramNumber, PMTPID: entry.PMTPID}
		for _, snapshot := range r.pmtHistory[entry.ProgramNumber] {
			if snapshot.EffectivePacket <= packet && snapshot.PID == entry.PMTPID {
				program.PMTRevision = snapshot.Revision
				program.PMTVersion = snapshot.Version
				program.PCRPID = snapshot.PCRPID
				program.Streams = append([]Stream(nil), snapshot.Streams...)
			}
		}
		ctx.Programs = append(ctx.Programs, program)
	}
	return ctx
}

func (r *Registry) currentPATPrograms() []PATProgram {
	if r.pat == nil {
		return nil
	}
	return r.pat.Programs
}

func (r *Registry) programPIDSet(program uint16) []uint16 {
	state := r.programs[program]
	if state == nil || state.Current == nil {
		return nil
	}
	pids := make([]uint16, 0, len(state.Current.Streams))
	for _, stream := range state.Current.Streams {
		pids = append(pids, stream.PID)
	}
	return sortedPIDSet(pids)
}

func (r *Registry) currentPIDSet() []uint16 {
	var pids []uint16
	for _, entry := range r.currentPATPrograms() {
		pids = append(pids, r.programPIDSet(entry.ProgramNumber)...)
	}
	return sortedPIDSet(pids)
}

func sortedPIDSet(pids []uint16) []uint16 {
	seen := make(map[uint16]bool)
	out := make([]uint16, 0, len(pids))
	for _, pid := range pids {
		if !seen[pid] {
			seen[pid] = true
			out = append(out, pid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func clonePIDs(pids []uint16) []uint16 {
	out := make([]uint16, len(pids))
	copy(out, pids)
	return out
}
