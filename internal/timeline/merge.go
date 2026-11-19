// Package timeline merges subtitle cues from several tracks onto the single
// archive tick grid. Ordering is deterministic and stable, and records that
// cannot be validated (zero duration or horizon overflow) are collected into
// a failure summary instead of being silently dropped.
package timeline

import (
	"sort"

	"archivetl/internal/clock"
)

// Item is a cue positioned on the archive grid. Priority breaks ties between
// overlapping cues: the smaller value wins. Track identifies the lane and is
// used as the final tie breaker so repeated runs produce identical output.
type Item struct {
	Track    string
	Source   string
	Sequence int
	Offset   int64
	Priority int
	Start    clock.Tick
	End      clock.Tick
	Text     string
}

// Event is an Item positioned in the merged order.
type Event struct {
	Item
	Order int // zero-based position in the merged timeline
	Lane  int // lane index assigned by the overlap packer
}

// Lane groups the events that share a visual track slot.
type Lane struct {
	Name   string
	Events []Event
}

// FailureKind enumerates the validation failures.
type FailureKind string

const (
	FailZeroDuration FailureKind = "zero_duration"
	FailOverflow     FailureKind = "overflow"
)

// Failure anchors a rejected or flagged record for later inspection.
type Failure struct {
	Kind     FailureKind
	Track    string
	Source   string
	Sequence int
	Offset   int64
	Start    clock.Tick
	End      clock.Tick
	Reason   string
}

// Summary is the validation result of one merge run.
type Summary struct {
	Accepted      int
	ZeroDuration  int
	Overflow      int
	Failures      []Failure
	RejectedCount int // records excluded from the merged timeline
}

// Merge orders items deterministically, packs them into non-overlapping
// lanes and validates durations against horizon. horizon <= 0 disables the
// overflow check. Overflowing items are excluded; zero-duration items are
// retained and flagged so reviewers can still locate them.
func Merge(items []Item, horizon clock.Tick) ([]Lane, *Summary) {
	sum := &Summary{}
	ordered := make([]Item, 0, len(items))
	for _, it := range items {
		if horizon > 0 && (it.Start > horizon || it.End > horizon) {
			sum.Overflow++
			sum.RejectedCount++
			sum.Failures = append(sum.Failures, Failure{
				Kind: FailOverflow, Track: it.Track, Source: it.Source,
				Sequence: it.Sequence, Offset: it.Offset,
				Start: it.Start, End: it.End,
				Reason: "cue extends beyond the configured horizon",
			})
			continue
		}
		if it.End <= it.Start {
			sum.ZeroDuration++
			sum.Failures = append(sum.Failures, Failure{
				Kind: FailZeroDuration, Track: it.Track, Source: it.Source,
				Sequence: it.Sequence, Offset: it.Offset,
				Start: it.Start, End: it.End,
				Reason: "cue has zero or negative duration",
			})
		}
		ordered = append(ordered, it)
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		return a.Sequence < b.Sequence
	})

	// Lane packing: each event takes the first lane whose last event ends no
	// later than the new start; otherwise a fresh lane is opened.
	type laneEnd struct {
		idx  int
		name string
		end  clock.Tick
	}
	var ends []laneEnd
	laneByName := map[string]int{}
	var lanes []Lane
	events := make([]Event, 0, len(ordered))
	for order, it := range ordered {
		placed := -1
		for k := range ends {
			if ends[k].end <= it.Start {
				placed = k
				break
			}
		}
		if placed < 0 {
			placed = len(ends)
			ends = append(ends, laneEnd{idx: placed, name: it.Track})
			lanes = append(lanes, Lane{Name: it.Track})
			if _, ok := laneByName[it.Track]; !ok {
				laneByName[it.Track] = placed
			}
		}
		ev := Event{Item: it, Order: order, Lane: placed}
		events = append(events, ev)
		ends[placed].end = it.End
	}
	for _, ev := range events {
		lanes[ev.Lane].Events = append(lanes[ev.Lane].Events, ev)
	}
	sum.Accepted = len(events)
	return lanes, sum
}

// Flatten returns the merged events in global order.
func Flatten(lanes []Lane) []Event {
	n := 0
	for _, l := range lanes {
		n += len(l.Events)
	}
	out := make([]Event, 0, n)
	for _, l := range lanes {
		out = append(out, l.Events...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}
