package rtp

import "github.com/example/mediad/internal/model"

// gap is one missing sequence number with its recovery deadline.
type gap struct {
	seq      uint16
	deadline uint16 // sequence number at/after which recovery is abandoned
}

// recoverer implements the finite recovery policy: a missing packet may
// only be repaired while the stream has advanced fewer than window
// packets past it. Repair means the original packet arriving late;
// beyond the window the gap is declared unrecoverable.
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
// It returns false once the gap has already expired (or never existed).
func (r *recoverer) noteLate(seq uint16) bool {
	if _, ok := r.gaps[seq]; !ok {
		return false
	}
	delete(r.gaps, seq)
	return true
}

// expire removes and returns every gap whose recovery window has
// elapsed relative to the highest sequence number observed.
func (r *recoverer) expire(maxSeq uint16) []uint16 {
	var out []uint16
	for seq := range r.gaps {
		if forwardDist(seq, maxSeq) >= int(r.window) {
			out = append(out, seq)
			delete(r.gaps, seq)
		}
	}
	return out
}

// expireAll abandons every still-open gap. Used at end of stream.
func (r *recoverer) expireAll() []uint16 {
	out := make([]uint16, 0, len(r.gaps))
	for seq := range r.gaps {
		out = append(out, seq)
	}
	r.gaps = map[uint16]gap{}
	return out
}

func (r *recoverer) pending() int { return len(r.gaps) }

// RecoveryOutcome classifies a packet-level loss event for reporting.
type RecoveryOutcome string

const (
	OutcomeRecovered    RecoveryOutcome = "recovered"
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
