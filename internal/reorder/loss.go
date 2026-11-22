package reorder

import (
	"fmt"

	"mediaroute/internal/fault"
)

// Gap is an inclusive range of missing sequence numbers on one track.
type Gap struct {
	Track string `json:"track"`
	From  uint32 `json:"from"`
	To    uint32 `json:"to"`
}

// Count returns the number of missing sequence numbers.
func (g Gap) Count() int { return int(g.To-g.From) + 1 }

// BurstThreshold is the minimum contiguous loss treated as a burst.
const BurstThreshold = 3

// Recovery applies the loss policy: gaps are marked and skipped, never
// filled with fabricated media. Bursts at or above BurstThreshold are
// counted separately so operators can confirm link-flap recovery.
type Recovery struct {
	Gaps            []Gap `json:"gaps,omitempty"`
	BurstsRecovered int   `json:"bursts_recovered"`
}

// Record marks a gap on a track and applies the recovery policy.
func (r *Recovery) Record(track string, g Gap) {
	g.Track = track
	r.Gaps = append(r.Gaps, g)
	if g.Count() >= BurstThreshold {
		r.BurstsRecovered++
	}
}

// RecordAll marks every gap from a window on the given track.
func (r *Recovery) RecordAll(track string, gaps []Gap) {
	for _, g := range gaps {
		r.Record(track, g)
	}
}

// Faults renders the recorded gaps as classified loss errors for reports.
func (r *Recovery) Faults() []*fault.Error {
	out := make([]*fault.Error, 0, len(r.Gaps))
	for _, g := range r.Gaps {
		out = append(out, fault.Newf(fault.CatLoss,
			"sequence gap %d..%d (%d packets)", g.From, g.To, g.Count()).
			WithTrack(g.Track).WithSeq(g.From))
	}
	return out
}

// String summarizes a gap for logs.
func (g Gap) String() string {
	return fmt.Sprintf("%s:%d..%d", g.Track, g.From, g.To)
}
