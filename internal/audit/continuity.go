// Package audit contains packet-level continuity and clock audits.
package audit

import (
	"sort"

	"tsaudit/internal/ts"
)

// ContinuityPIDStats is the per-PID continuity ledger.
type ContinuityPIDStats struct {
	PID             uint16 `json:"pid"`
	Packets         int64  `json:"packets"`
	LossPackets     int64  `json:"loss_packets"`
	CCJumps         int64  `json:"cc_jumps"`
	Duplicates      int64  `json:"duplicates"`
	Discontinuities int64  `json:"discontinuities"`
}

// ContinuityStats is the aggregate continuity report.
type ContinuityStats struct {
	LossPackets     int64                `json:"loss_packets"`
	CCJumps         int64                `json:"cc_jumps"`
	Duplicates      int64                `json:"duplicates"`
	Discontinuities int64                `json:"discontinuities"`
	PerPID          []ContinuityPIDStats `json:"per_pid"`
}

type continuityPID struct {
	stats ContinuityPIDStats
	last  uint8
	have  bool
}

// Continuity tracks CC modulo 16 independently for every non-null PID.
type Continuity struct {
	byPID map[uint16]*continuityPID
}

// NewContinuity creates an empty continuity auditor.
func NewContinuity() *Continuity { return &Continuity{byPID: make(map[uint16]*continuityPID)} }

// Observe updates the state for a packet. Packets without a payload do not
// advance continuity_counter; null PID packets are excluded.
func (c *Continuity) Observe(p *ts.Packet) {
	if p.Header.PID == ts.NullPID {
		return
	}
	if !p.Header.HasPayload {
		if p.Header.AdaptationField && p.Adapt.DiscontinuityIndicator {
			st := c.byPID[p.Header.PID]
			if st == nil {
				st = &continuityPID{stats: ContinuityPIDStats{PID: p.Header.PID}}
				c.byPID[p.Header.PID] = st
			}
			st.stats.Discontinuities++
			st.last, st.have = p.Header.ContinuityCounter, true
		}
		return
	}
	st := c.byPID[p.Header.PID]
	if st == nil {
		st = &continuityPID{stats: ContinuityPIDStats{PID: p.Header.PID}}
		c.byPID[p.Header.PID] = st
	}
	st.stats.Packets++
	cc := p.Header.ContinuityCounter
	if p.Header.AdaptationField && p.Adapt.DiscontinuityIndicator {
		st.stats.Discontinuities++
		st.last, st.have = cc, true
		return
	}
	if !st.have {
		st.last, st.have = cc, true
		return
	}
	delta := (int(cc) - int(st.last) + 16) & 0x0f
	switch {
	case delta == 1:
		st.last = cc
	case delta == 0:
		st.stats.Duplicates++
	case delta > 8:
		st.stats.CCJumps++
		st.last = cc
	default:
		st.stats.LossPackets += int64(delta - 1)
		st.last = cc
	}
}

// Stats returns a sorted aggregate snapshot.
func (c *Continuity) Stats() ContinuityStats {
	out := ContinuityStats{}
	for _, st := range c.byPID {
		out.LossPackets += st.stats.LossPackets
		out.CCJumps += st.stats.CCJumps
		out.Duplicates += st.stats.Duplicates
		out.Discontinuities += st.stats.Discontinuities
		out.PerPID = append(out.PerPID, st.stats)
	}
	sort.Slice(out.PerPID, func(i, j int) bool { return out.PerPID[i].PID < out.PerPID[j].PID })
	return out
}
