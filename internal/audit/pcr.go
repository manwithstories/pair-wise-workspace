package audit

import (
	"math"
	"sort"

	"tsaudit/internal/ts"
)

const pcrWrapTicks = int64(1) << 33 * 300

// PCRStats reports PCR cadence for one PID.
type PCRStats struct {
	PID             uint16  `json:"pid"`
	Count           int64   `json:"count"`
	WrapCount       int64   `json:"wrap_count"`
	Discontinuities int64   `json:"discontinuities"`
	MinIntervalMS   float64 `json:"min_interval_ms,omitempty"`
	MaxIntervalMS   float64 `json:"max_interval_ms,omitempty"`
	MeanIntervalMS  float64 `json:"mean_interval_ms,omitempty"`
	MaxJitterMS     float64 `json:"max_jitter_ms,omitempty"`
	FirstPCR        int64   `json:"first_pcr"`
	LastPCR         int64   `json:"last_pcr"`
}

type pcrPID struct {
	stats     PCRStats
	lastTicks int64
	have      bool
	intervals []float64
}

// PCRCheck measures PCR intervals and detects 33-bit base wrap.
type PCRCheck struct{ byPID map[uint16]*pcrPID }

// NewPCRCheck creates an empty PCR auditor.
func NewPCRCheck() *PCRCheck { return &PCRCheck{byPID: make(map[uint16]*pcrPID)} }

// Observe consumes a packet carrying a PCR.
func (c *PCRCheck) Observe(p *ts.Packet) {
	if !p.Header.AdaptationField || !p.Adapt.HasPCR {
		return
	}
	st := c.byPID[p.Header.PID]
	if st == nil {
		st = &pcrPID{stats: PCRStats{PID: p.Header.PID}}
		c.byPID[p.Header.PID] = st
	}
	st.stats.Count++
	ticks := p.Adapt.PCRBase*300 + p.Adapt.PCRExtension
	if st.stats.Count == 1 {
		st.stats.FirstPCR = p.Adapt.PCRBase
	}
	st.stats.LastPCR = p.Adapt.PCRBase
	if !st.have {
		st.lastTicks, st.have = ticks, true
		return
	}
	if p.Adapt.DiscontinuityIndicator {
		st.stats.Discontinuities++
		st.lastTicks = ticks
		return
	}
	diff := ticks - st.lastTicks
	if diff < 0 {
		diff += pcrWrapTicks
		if diff >= 0 {
			st.stats.WrapCount++
		}
	}
	if diff >= 0 && diff < pcrWrapTicks/2 {
		st.intervals = append(st.intervals, float64(diff)/27000.0)
	}
	st.lastTicks = ticks
}

// Stats finalizes interval statistics and returns them sorted by PID.
func (c *PCRCheck) Stats() []PCRStats {
	out := make([]PCRStats, 0, len(c.byPID))
	for _, st := range c.byPID {
		if len(st.intervals) > 0 {
			min, max, sum := st.intervals[0], st.intervals[0], 0.0
			for _, v := range st.intervals {
				if v < min {
					min = v
				}
				if v > max {
					max = v
				}
				sum += v
			}
			mean := sum / float64(len(st.intervals))
			jitter := 0.0
			for _, v := range st.intervals {
				if d := math.Abs(v - mean); d > jitter {
					jitter = d
				}
			}
			st.stats.MinIntervalMS, st.stats.MaxIntervalMS = min, max
			st.stats.MeanIntervalMS, st.stats.MaxJitterMS = mean, jitter
		}
		out = append(out, st.stats)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}
