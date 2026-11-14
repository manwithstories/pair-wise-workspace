package psi

import (
	"fmt"
	"strings"
)

type Change struct {
	Table       string `json:"table"`
	Program     uint16 `json:"program,omitempty"`
	OldVersion  uint8  `json:"old_version"`
	NewVersion  uint8  `json:"new_version"`
	ContentHash string `json:"content_hash"`
	Offset      int64  `json:"offset"`
	SnapshotID  int    `json:"snapshot_id"`
	Initial     bool   `json:"initial"`
}

type identity struct {
	table   uint8
	program uint16
	version uint8
	hash    [32]byte
}

// Registry owns the active PAT/PMT state. It emits immutable snapshots and
// resolves version changes without allowing candidate tables to take effect.
type Registry struct {
	PMTTimeoutPackets int64
	patMerger         PATMerger
	pmtMergers        map[uint16]*PMTMerger
	activePAT         *PAT
	activePMTs        map[uint16]*PMT
	seen              map[identity]struct{}
	patActivations    int
	pmtActivations    map[uint16]int
	pendingSince      map[uint16]int64
	pendingWarned     map[uint16]bool
	candidates        map[string][32]byte
	packetIndex       int64
	nextSnapshotID    int
	currentSnapshot   Snapshot
	Changes           []Change
	Conflicts         []Conflict
	Warnings          []string
}

func NewRegistry(pmtTimeoutPackets int64) *Registry {
	if pmtTimeoutPackets <= 0 {
		pmtTimeoutPackets = 2000
	}
	return &Registry{
		PMTTimeoutPackets: pmtTimeoutPackets,
		pmtMergers:        make(map[uint16]*PMTMerger),
		activePMTs:        make(map[uint16]*PMT),
		seen:              make(map[identity]struct{}),
		pmtActivations:    make(map[uint16]int),
		pendingSince:      make(map[uint16]int64),
		pendingWarned:     make(map[uint16]bool),
		candidates:        make(map[string][32]byte),
	}
}

func (r *Registry) Tick(packetIndex int64) {
	r.packetIndex = packetIndex
	for program, since := range r.pendingSince {
		if !r.pendingWarned[program] && packetIndex-since >= r.PMTTimeoutPackets {
			r.pendingWarned[program] = true
			r.Warnings = append(r.Warnings, fmt.Sprintf("program %d: PMT pending for %d packets", program, packetIndex-since))
		}
	}
}

func (r *Registry) HandleSection(sec Section) {
	if !sec.CurrentNext {
		key := fmt.Sprintf("%d:%d", sec.TableID, sec.TableIDExtension)
		r.candidates[key] = sec.Hash
		return
	}
	switch sec.TableID {
	case TablePAT:
		r.handlePAT(sec)
	case TablePMT:
		r.handlePMT(sec)
	}
}

func (r *Registry) handlePAT(sec Section) {
	pat, complete, err := r.patMerger.Add(sec)
	if err != nil || !complete {
		return
	}
	id := identity{table: TablePAT, version: pat.Version, hash: pat.ContentHash}
	if _, ok := r.seen[id]; ok {
		return
	}
	r.seen[id] = struct{}{}
	oldVersion := uint8(0)
	if r.activePAT != nil {
		oldVersion = r.activePAT.Version
	}
	previousPrograms := make(map[uint16]uint16)
	if r.activePAT != nil {
		for program, pid := range r.activePAT.Programs {
			previousPrograms[program] = pid
		}
	}
	r.activePAT = pat
	r.patActivations++
	r.syncPendingPrograms(previousPrograms)
	initial := r.patActivations == 1
	change := Change{
		Table: "PAT", OldVersion: oldVersion, NewVersion: pat.Version,
		ContentHash: shortHash(pat.ContentHash), Offset: sec.EndOffset,
		Initial: initial,
	}
	r.activateSnapshot(change, sec.EndOffset)
}

func (r *Registry) handlePMT(sec Section) {
	program := sec.TableIDExtension
	if r.activePAT == nil {
		return
	}
	if _, announced := r.activePAT.Programs[program]; !announced {
		return // A PMT for a program absent from the current PAT is frozen out.
	}
	merger := r.pmtMergers[program]
	if merger == nil {
		merger = &PMTMerger{}
		r.pmtMergers[program] = merger
	}
	pmt, complete, err := merger.Add(sec)
	if err != nil || !complete {
		return
	}
	id := identity{table: TablePMT, program: program, version: pmt.Version, hash: pmt.ContentHash}
	if _, ok := r.seen[id]; ok {
		return
	}
	r.seen[id] = struct{}{}
	oldVersion := uint8(0)
	if previous := r.activePMTs[program]; previous != nil {
		oldVersion = previous.Version
		for _, oldES := range previous.Streams {
			for _, newES := range pmt.Streams {
				if oldES.PID == newES.PID && oldES.StreamType != newES.StreamType {
					r.Conflicts = append(r.Conflicts, Conflict{
						PID: oldES.PID, Program: program,
						Message: fmt.Sprintf("stream_type changed for PID 0x%04x (%d -> %d)", oldES.PID, oldES.StreamType, newES.StreamType),
					})
				}
			}
		}
	}
	r.activePMTs[program] = pmt
	r.pmtActivations[program]++
	delete(r.pendingSince, program)
	initial := r.pmtActivations[program] == 1
	change := Change{
		Table: "PMT", Program: program, OldVersion: oldVersion, NewVersion: pmt.Version,
		ContentHash: shortHash(pmt.ContentHash), Offset: sec.EndOffset, Initial: initial,
	}
	r.activateSnapshot(change, sec.EndOffset)
}

func (r *Registry) syncPendingPrograms(previous map[uint16]uint16) {
	if r.activePAT == nil {
		return
	}
	announced := make(map[uint16]bool)
	for program, pmtPID := range r.activePAT.Programs {
		announced[program] = true
		if oldPID, existed := previous[program]; existed && oldPID != pmtPID {
			delete(r.activePMTs, program)
			r.forgetPMT(program)
			r.pendingSince[program] = r.packetIndex
			r.pendingWarned[program] = false
		}
		if _, started := r.pendingSince[program]; !started {
			if _, received := r.activePMTs[program]; !received {
				r.pendingSince[program] = r.packetIndex
			}
		}
	}
	for program := range r.pendingSince {
		if !announced[program] {
			delete(r.pendingSince, program)
		}
	}
}

func (r *Registry) forgetPMT(program uint16) {
	for id := range r.seen {
		if id.table == TablePMT && id.program == program {
			delete(r.seen, id)
		}
	}
}

func (r *Registry) activateSnapshot(change Change, offset int64) {
	r.nextSnapshotID++
	snap := r.buildSnapshot(r.nextSnapshotID, offset)
	r.currentSnapshot = snap
	change.SnapshotID = snap.ID
	r.Changes = append(r.Changes, change)
	r.checkPSIOverlap(snap.ID)
	for i := range r.Conflicts {
		if r.Conflicts[i].SnapshotID == 0 {
			r.Conflicts[i].SnapshotID = snap.ID
		}
	}
}

func (r *Registry) IsPMTPID(pid uint16) bool {
	if r.activePAT == nil {
		return false
	}
	for _, candidate := range r.activePAT.Programs {
		if candidate == pid {
			return true
		}
	}
	return false
}

func (r *Registry) PATVersionChanges() int {
	if r.patActivations == 0 {
		return 0
	}
	return r.patActivations - 1
}

func (r *Registry) PMTVersionChanges() map[uint16]int {
	out := make(map[uint16]int)
	for program, count := range r.pmtActivations {
		if count > 0 {
			out[program] = count - 1
		}
	}
	return out
}

func (r *Registry) Finalize() {
	for program := range r.pendingSince {
		if r.pendingWarned[program] {
			continue
		}
		r.Warnings = append(r.Warnings, fmt.Sprintf("program %d: PMT still pending at end of stream", program))
	}
}

func shortHash(h [32]byte) string {
	return strings.ToLower(fmt.Sprintf("%x", h[:6]))
}
