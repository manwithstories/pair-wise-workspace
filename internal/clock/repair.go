package clock

import "fmt"

// Status describes the outcome of repairing one segment timestamp.
type Status int

const (
	// StatusOK means the observed timestamp matched the expected timeline.
	StatusOK Status = iota
	// StatusRepaired means the timestamp was deterministically remapped.
	StatusRepaired
	// StatusUnrecoverable means no deterministic mapping exists; the
	// segment must be excluded from the index.
	StatusUnrecoverable
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusRepaired:
		return "repaired"
	default:
		return "unrecoverable"
	}
}

// Repair records one deterministic timestamp remapping.
type Repair struct {
	Segment  int
	Observed uint64
	Adjusted uint64
	Reason   string
}

// Repairer rebuilds a monotonic 90 kHz timeline from per-segment observed
// start timestamps. The mapping is deterministic: given the same declared
// durations and observed starts it always produces the same adjustments.
type Repairer struct {
	tolerance uint64
	next      uint64
	started   bool
}

// NewRepairer creates a Repairer accepting deviations up to tolerance ticks.
func NewRepairer(tolerance uint64) *Repairer { return &Repairer{tolerance: tolerance} }

// Add folds one segment into the timeline. observed is the segment's
// timestamp as read from the media, dur its declared duration in ticks.
// It returns the adjusted start, the repair status, and a Repair record
// when the status is not StatusOK.
func (r *Repairer) Add(segment int, observed, dur uint64) (uint64, Status, *Repair) {
	if !r.started {
		r.started = true
		r.next = observed + dur
		return observed, StatusOK, nil
	}
	exp := r.next
	if observed >= exp && observed-exp <= r.tolerance {
		r.next = exp + dur
		return exp, StatusOK, nil
	}
	if observed < exp && exp-observed <= r.tolerance {
		r.next = exp + dur
		return exp, StatusOK, nil
	}
	r.next = exp + dur
	switch {
	case observed < exp:
		// Regression or duplicate (e.g. encoder restart): snap forward.
		rep := &Repair{Segment: segment, Observed: observed, Adjusted: exp, Reason: "backward-or-duplicate-timestamp"}
		return exp, StatusRepaired, rep
	default:
		gap := observed - exp
		if dur > 0 && gap%dur == 0 {
			// Whole segments dropped: the offset stays determinable.
			rep := &Repair{Segment: segment, Observed: observed, Adjusted: exp,
				Reason: fmt.Sprintf("gap of %d whole segment(s) closed", gap/dur)}
			return exp, StatusRepaired, rep
		}
		// Ambiguous gap: cannot determine the true offset. Resynchronise
		// the expectation on the observed start so later segments continue.
		r.next = observed + dur
		return observed, StatusUnrecoverable, &Repair{
			Segment: segment, Observed: observed, Adjusted: observed,
			Reason: fmt.Sprintf("ambiguous gap of %d ticks", gap),
		}
	}
}

// wrap33 is the 33-bit MPEG PTS/DTS modulus.
const wrap33 = uint64(1) << 33

// Unwrap33 maps a raw 33-bit timestamp onto the 64-bit timeline using ref
// (a nearby reference tick) to resolve wraparound. It returns the value
// congruent to raw mod 2^33 closest to ref.
func Unwrap33(ref, raw uint64) uint64 {
	raw &= wrap33 - 1
	base := ref &^ (wrap33 - 1)
	cand := base | raw
	switch {
	case cand+wrap33/2 < ref:
		return cand + wrap33
	case ref+wrap33/2 < cand:
		return cand - wrap33
	default:
		return cand
	}
}
