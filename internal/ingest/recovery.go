package ingest

// Interval is an inclusive extended-sequence range.
type Interval struct {
	Start uint32 `json:"start"`
	End   uint32 `json:"end"`
}

// LossTracker records burst loss as explicit missing-packet intervals
// plus duplicate and late-packet counters.
type LossTracker struct {
	Intervals []Interval `json:"intervals"`
	Dups      int        `json:"dups"`
	Late      int        `json:"late"`
	missing   int
}

// AddMissing records one lost packet, merging adjacent sequence
// numbers into a single burst interval.
func (t *LossTracker) AddMissing(seq uint32) {
	t.missing++
	if n := len(t.Intervals); n > 0 && t.Intervals[n-1].End+1 == seq {
		t.Intervals[n-1].End = seq
		return
	}
	t.Intervals = append(t.Intervals, Interval{Start: seq, End: seq})
}

// AddDup records a duplicate packet.
func (t *LossTracker) AddDup() { t.Dups++ }

// AddLate records a packet that arrived outside the reorder window.
func (t *LossTracker) AddLate() { t.Late++ }

// Missing is the total number of lost packets.
func (t *LossTracker) Missing() int { return t.missing }
