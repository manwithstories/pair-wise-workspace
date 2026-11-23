package rtp

import "github.com/example/mediad/internal/model"

// gap is one missing sequence number with its recovery deadline.
type gap struct {
	seq      uint16
	deadline uint16 // once the stream reaches this seq, recovery is over
}

// recoverer implements the finite recovery policy: a missing packet may
// only be repaired while the stream has advanced fewer than window
// packets past it. Repair means the original packet arriving late;
// beyond the deadline the gap is declared unrecoverable.
type recoverer struct {
	window uint16
	gaps   map[uint16]gap
}

func newRecoverer(window uint16) *recoverer {
	return &recoverer{window: window, gaps: map[uint16]gap{}}
}

// registerGap records every sequence number in (prev, cur).
func (r *recoverer) registerGap(prev, cur uint16) {
	for s := prev + 1; s != cur; s++ {
		if _, ok := r.gaps[s]; !ok {
			r.gaps[s] = gap{seq: s, deadline: s + r.window}
		}
	}
}

// noteLate reports that the packet for a missing seq has arrived.
// It returns false once the gap has expired (or never existed).
func (r *recoverer) noteLate(seq uint16) bool {
	if _, ok := r.gaps[seq]; !ok {
		return false
	}
	delete(r.gaps, seq)
	return true
}

// expire removes and returns gaps whose deadlines the stream has now
// reached (or passed), given the highest sequence number observed.
func (r *recoverer) expire(maxSeq uint16) []uint16 {
	var out []uint16
	for seq, g := range r.gaps {
		if forwardDist(g.deadline, maxSeq) >= 0 {
			out = append(out, seq)
			delete(r.gaps, seq)
		}
	}
	return out
}

// expireAt forces expiration of all gaps at or below seq regardless of
// their remaining window. Used when the buffer must advance past a
// blocked base position.
func (r *recoverer) expireAt(seq uint16) []uint16 {
	var out []uint16
	for s := range r.gaps {
		if forwardDist(s, seq) <= 0 {
			out = append(out, s)
			delete(r.gaps, s)
		}
	}
	return out
}

func (r *recoverer) pending() int { return len(r.gaps) }

// RecoveryOutcome classifies a packet-level loss event for reporting.
type RecoveryOutcome string

const (
	OutcomeRecovered     RecoveryOutcome = "recovered"
	OutcomeUnrecoverable RecoveryOutcome = "unrecoverable"
)

// Classify maps the buffer counters to per-outcome totals and returns a
// checkable error when unrecoverable loss remains.
func Classify(stats Stats) (map[RecoveryOutcome]int, error) {
	out := map[RecoveryOutcome]int{
		OutcomeRecovered:     stats.Recovered,
		OutcomeUnrecoverable: stats.UnrecoverablePackets,
	}
	if stats.UnrecoverablePackets > 0 {
		return out, model.New(model.KindCorrupt, "rtp", "recover",
			"packets lost beyond the recovery window; segments isolated")
	}
	return out, nil
}
