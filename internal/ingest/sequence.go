package ingest

import (
	"fmt"

	"mediacheck/internal/model"
)

// seqWindow is the width of the sequence counter.  RTP and Matroska block
// sequence numbers both use 16 bits and wrap at 65536.
const seqWindow = 1 << 16

// dedupHistory is how many recently accepted sequence numbers a tracker
// remembers for duplicate detection.
const dedupHistory = 256

// SequenceTracker inspects the sequence numbers of one track.  It is
// deliberately tolerant of damaged input: a duplicate is reported once and
// dropped, a wrap is reported and scanning continues, and a regression is
// reported with an explicit offset instead of resetting the cursor.
type SequenceTracker struct {
	track       model.Track
	started     bool
	highest     uint64 // highest extended sequence accepted so far
	wraps       int
	seen        map[uint32]struct{}
	order       []uint32
	Accepted    int
	Duplicates  int
	Lost        int
	Regressions int
	WrapCount   int
}

// NewSequenceTracker returns a tracker for track.
func NewSequenceTracker(track model.Track) *SequenceTracker {
	return &SequenceTracker{
		track: track,
		seen:  make(map[uint32]struct{}, dedupHistory),
	}
}

// ObserveResult is what the tracker learned from one sequence number.
type ObserveResult struct {
	Duplicate bool
	Wrapped   bool
	Lost      int   // number of missing sequence numbers before this one
	Offset    int64 // negative when the timestamp moved backwards
}

// Base returns the extended sequence number (32 bit counter plus wrap
// count) of the last accepted packet.
func (t *SequenceTracker) Base() uint64 { return t.highest }

// Observe feeds the raw 16 bit sequence number of the next packet.  It
// returns the verdict without mutating packets, so callers decide what to do
// with duplicates.
func (t *SequenceTracker) Observe(seq uint32) ObserveResult {
	raw := seq % seqWindow
	var res ObserveResult

	// The dedup set only retains the last dedupHistory sequence numbers, so
	// membership means "a recent retransmission".  A sequence that is old
	// enough to have fallen out is not a duplicate: it is either a fresh
	// counter cycle (handled by extend) or a regression.
	if t.started {
		if _, dup := t.seen[raw]; dup {
			t.Duplicates++
			res.Duplicate = true
			return res
		}
	}

	extended, wrapped := t.extend(raw)
	res.Wrapped = wrapped
	if t.started {
		switch {
		case extended == t.highest:
			t.Duplicates++
			res.Duplicate = true
			return res
		case extended < t.highest:
			// Timestamp/sequence regression: report the offset and keep
			// scanning from the highest point seen so far.
			t.Regressions++
			res.Offset = int64(extended) - int64(t.highest)
		case extended > t.highest:
			res.Lost = int(extended - t.highest - 1)
			t.Lost += res.Lost
		}
	}
	if extended > t.highest {
		t.highest = extended
	}
	t.started = true
	t.remember(raw)
	t.Accepted++
	return res
}

// extend maps a 16 bit sequence number onto the current wrap epoch and
// reports whether the counter wrapped at this observation.
func (t *SequenceTracker) extend(raw uint32) (uint64, bool) {
	if !t.started {
		return uint64(raw), false
	}
	base := t.highest % seqWindow
	candidate := t.highest - base + uint64(raw)
	// A jump of more than half the window backwards is really a forward
	// wrap of the counter, not a regression.
	if candidate+seqWindow/2 < t.highest {
		t.wraps++
		t.WrapCount++
		return candidate + seqWindow, true
	}
	return candidate, false
}

func (t *SequenceTracker) remember(raw uint32) {
	if _, ok := t.seen[raw]; ok {
		return
	}
	t.seen[raw] = struct{}{}
	t.order = append(t.order, raw)
	if len(t.order) > dedupHistory {
		delete(t.seen, t.order[0])
		t.order = t.order[1:]
	}
}

// Gap describes one missing sequence range.
type Gap struct {
	Track model.Track
	First uint32 // first missing raw sequence number
	Count int
	Slot  model.Slot
}

// String renders the gap for logs.
func (g Gap) String() string {
	return fmt.Sprintf("%s gap seq=%d count=%d %s", g.Track, g.First, g.Count, g.Slot)
}

// ExpectedSlots lists the timeline slots that a gap of count packets covers,
// using the mean packet span of surrounding accepted packets.  It lets
// recover fill exactly the missing slots instead of guessing.
func ExpectedSlots(prev, next model.Packet, count int) []model.Slot {
	if count <= 0 {
		return nil
	}
	span := next.Slot.Start - prev.Slot.End
	if span <= 0 || prev.Slot.Duration() <= 0 {
		// Fall back to the previous packet's own duration.
		per := prev.Slot.Duration()
		if per <= 0 {
			return nil
		}
		slots := make([]model.Slot, 0, count)
		start := prev.Slot.End
		for i := 0; i < count; i++ {
			slots = append(slots, model.Slot{Start: start, End: start + per})
			start += per
		}
		return slots
	}
	per := span / int64(count)
	if per <= 0 {
		return nil
	}
	slots := make([]model.Slot, 0, count)
	start := prev.Slot.End
	for i := 0; i < count-1; i++ {
		slots = append(slots, model.Slot{Start: start, End: start + per})
		start += per
	}
	slots = append(slots, model.Slot{Start: start, End: next.Slot.Start})
	return slots
}
