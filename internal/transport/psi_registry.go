package transport

import (
	"errors"
	"fmt"
)

type psiKey struct {
	pid     uint16
	tableID byte
}

type pendingPSI struct {
	table   *PSITable
	matches int
}

// PSIRegistry retains last-known-good PSI and counts rejected tables.
type PSIRegistry struct {
	PAT                   *PSITable
	PMTs                  map[uint16]*PSITable
	KnownPMTPIDs          map[uint16]bool
	BadCRC                int
	Malformed             int
	Applied               []*PSITable
	RequiredConfirmations int
	pending               map[psiKey]pendingPSI
}

func NewPSIRegistry() *PSIRegistry {
	return &PSIRegistry{
		PMTs:                  map[uint16]*PSITable{},
		KnownPMTPIDs:          map[uint16]bool{},
		RequiredConfirmations: 2,
		pending:               map[psiKey]pendingPSI{},
	}
}

// HandlePacket parses PAT/PMT packets. A bad-CRC update increments BadCRC and
// returns ErrBadCRC while preserving the previous valid version.
func (r *PSIRegistry) HandlePacket(p Packet) (*PSITable, error) {
	if p.PID != 0 && !r.KnownPMTPIDs[p.PID] {
		return nil, nil
	}
	t, err := ParsePSIPayload(p.PID, p.Payload)
	if err != nil {
		if errors.Is(err, ErrBadCRC) {
			r.BadCRC++
		} else {
			r.Malformed++
		}
		return nil, err
	}

	key := psiKey{pid: p.PID, tableID: t.TableID}
	current := r.currentTable(key)
	if current != nil && current.Version == t.Version && current.ContentHash == t.ContentHash {
		return current, nil
	}

	need := r.RequiredConfirmations
	if need < 1 {
		need = 1
	}
	pend := r.pending[key]
	if pend.table != nil && (pend.table.Version != t.Version || pend.table.ContentHash != t.ContentHash) {
		delete(r.pending, key)
		return nil, fmt.Errorf("%w: pid=0x%x pending v%d conflicts with v%d",
			ErrVersionConflict, p.PID, pend.table.Version, t.Version)
	}
	if pend.table == nil {
		pend = pendingPSI{table: t, matches: 1}
	} else {
		pend.matches++
	}
	if pend.matches < need {
		r.pending[key] = pend
		return t, ErrPSIWaitingConfirm
	}

	delete(r.pending, key)
	r.setCurrent(t)
	r.Applied = append(r.Applied, t)
	if t.TableID == 0x00 {
		progs, err := ParsePATEntries(t)
		if err == nil {
			for _, prog := range progs {
				r.KnownPMTPIDs[prog.PMTPID] = true
			}
		}
	}
	return t, nil
}

func (r *PSIRegistry) currentTable(key psiKey) *PSITable {
	if key.tableID == 0x00 && key.pid == 0 {
		return r.PAT
	}
	return r.PMTs[key.pid]
}

func (r *PSIRegistry) setCurrent(t *PSITable) {
	if t.TableID == 0x00 && t.PID == 0 {
		r.PAT = t
	} else {
		r.PMTs[t.PID] = t
	}
}

// PMTVersion returns the active PMT version for a PID, or -1 when absent.
func (r *PSIRegistry) PMTVersion(pmtPID uint16) int {
	if t := r.PMTs[pmtPID]; t != nil {
		return int(t.Version)
	}
	return -1
}
