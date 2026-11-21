package rtp

import (
	"sort"

	"mediad/internal/model"
)

// Gap is a contiguous run of sequence numbers that never arrived.
type Gap struct {
	// FirstSeq and LastSeq are inclusive, in unwrapped stream coordinates.
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	Count    int    `json:"count"`
	// Start and End are the media-time bounds of the gap, derived from the
	// packets on either side. They are zero when no neighbour exists.
	StartTicks uint32 `json:"start_ticks"`
	EndTicks   uint32 `json:"end_ticks"`
	// Recoverable is false when the gap spans a keyframe boundary or is too
	// long to bridge without inventing data.
	Recoverable bool `json:"recoverable"`
	// Bridged counts packets whose timing is covered by an intact neighbour.
	Bridged int `json:"bridged"`
}

// LossReport summarises burst loss for the manifest and statistics stages.
type LossReport struct {
	Gaps []Gap `json:"gaps"`
	// UnrecoverableSamples is the number of samples that cannot be presented
	// because their bytes are missing.
	UnrecoverableSamples int `json:"unrecoverable_samples"`
	// RecoveredSamples is the number of samples whose timing was restored
	// from intact neighbours. Their bytes are still absent, so they are
	// marked Synthesised and never reported as original.
	RecoveredSamples int `json:"recovered_samples"`
	// TotalLost counts missing sequence numbers.
	TotalLost int `json:"total_lost"`
}

// ClassifyLoss turns the receiver's missing sequences into explicit,
// presented gaps. A gap is only marked recoverable when it is a short run
// bounded by packets that carry media time, and when none of the delivered
// packets inside the same frame are keyframes.
func ClassifyLoss(stats Stats, delivered []Packet, maxBurst int) LossReport {
	if maxBurst <= 0 {
		maxBurst = 16
	}
	var rep LossReport
	rep.TotalLost = len(stats.MissingSeqs)
	if rep.TotalLost == 0 {
		return rep
	}
	missing := append([]uint64(nil), stats.MissingSeqs...)
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })

	bySeq := make(map[uint64]Packet, len(delivered))
	for _, p := range delivered {
		bySeq[p.Seq] = p
	}

	for i := 0; i < len(missing); {
		j := i
		for j+1 < len(missing) && missing[j+1] == missing[j]+1 {
			j++
		}
		g := Gap{FirstSeq: missing[i], LastSeq: missing[j], Count: j - i + 1}
		before, hasBefore := bySeq[g.FirstSeq-1]
		after, hasAfter := bySeq[g.LastSeq+1]
		switch {
		case hasBefore && hasAfter:
			g.StartTicks = before.Timestamp
			g.EndTicks = after.Timestamp
			g.Recoverable = g.Count <= maxBurst && before.Timestamp < after.Timestamp
			g.Bridged = g.Count
		case hasBefore:
			g.StartTicks = before.Timestamp
			g.EndTicks = before.Timestamp
		case hasAfter:
			g.StartTicks = after.Timestamp
			g.EndTicks = after.Timestamp
		}
		if g.Recoverable {
			rep.RecoveredSamples += g.Bridged
		} else {
			rep.UnrecoverableSamples += g.Count
		}
		rep.Gaps = append(rep.Gaps, g)
		i = j + 1
	}
	return rep
}

// DamagedRanges converts unrecoverable gaps into sample-damage flags on the
// track. Samples whose timestamp falls inside an unrecoverable gap are
// marked Damaged; recoverable gaps are marked Synthesised so downstream code
// can tell a rebuilt timestamp from a delivered frame.
func DamagedRanges(tr *model.Track, rep LossReport) error {
	if len(rep.Gaps) == 0 {
		return nil
	}
	for _, g := range rep.Gaps {
		for i := range tr.Samples {
			s := &tr.Samples[i]
			ticks := uint32(s.DTS.Float64() * float64(tr.Timescale))
			if ticks < g.StartTicks || ticks > g.EndTicks {
				continue
			}
			if g.Recoverable {
				s.Synthesised = true
			} else {
				s.Damaged = true
			}
		}
	}
	return nil
}
