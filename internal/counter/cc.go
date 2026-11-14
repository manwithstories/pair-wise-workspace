// Package counter tracks MPEG-TS continuity counters per PID.
package counter

import "tsaudit/internal/tspipe"

const NullPID uint16 = 0x1fff

type Jump struct {
	PID        uint16 `json:"pid"`
	Offset     int64  `json:"offset"`
	Previous   uint8  `json:"previous"`
	Next       uint8  `json:"next"`
	Lost       int    `json:"estimated_lost"`
	SnapshotID int    `json:"snapshot_id"`
}

type state struct {
	seen    bool
	lastCC  uint8
	lastSum uint64
}

type Tracker struct {
	states     map[uint16]*state
	Jumps      []Jump
	Duplicates int
	Lost       int
}

func NewTracker() *Tracker {
	return &Tracker{states: make(map[uint16]*state)}
}

// Observe updates one PID state. Adaptation-only packets and null packets are
// ignored. A duplicate is only recognized when both CC and payload match.
func (t *Tracker) Observe(p *tspipe.Packet) *Jump {
	if p == nil || !p.HasPayload || p.PID == NullPID || p.TEI {
		return nil
	}
	st := t.states[p.PID]
	if st == nil {
		st = &state{}
		t.states[p.PID] = st
	}
	sum := payloadSum(p.Payload)
	if !st.seen || p.Discontinuity {
		st.seen = true
		st.lastCC = p.Continuity
		st.lastSum = sum
		return nil
	}
	if p.Continuity == st.lastCC && sum == st.lastSum {
		t.Duplicates++
		return nil
	}
	previous := st.lastCC
	expected := (st.lastCC + 1) & 0x0f
	delta := int((p.Continuity - expected + 16) & 0x0f)
	st.lastCC = p.Continuity
	st.lastSum = sum
	if delta == 0 {
		return nil
	}
	jump := Jump{
		PID: p.PID, Offset: p.Offset, Previous: previous,
		Next: p.Continuity, Lost: delta,
	}
	t.Jumps = append(t.Jumps, jump)
	t.Lost += delta
	return &t.Jumps[len(t.Jumps)-1]
}

func payloadSum(data []byte) uint64 {
	const offset64 = 1469598103934665603
	const prime64 = 1099511628211
	h := uint64(offset64)
	for _, b := range data {
		h ^= uint64(b)
		h *= prime64
	}
	return h
}
