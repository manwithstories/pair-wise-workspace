package timeline

import (
	"fmt"

	"example.com/medialab/archive/internal/model"
)

// SegmentIndex returns the grid index containing t.
func SegmentIndex(t, segUS int64) int {
	if t < 0 {
		return 0
	}
	return int(t / segUS)
}

// Boundary returns the grid boundary before index i.
func Boundary(i int, segUS int64) int64 { return int64(i) * segUS }

// AffectedSegments lists the segment indices that a half-open interval touches.
func AffectedSegments(startUS, endUS int64, segUS int64) []int {
	if endUS <= startUS {
		endUS = startUS + 1
	}
	first := SegmentIndex(startUS, segUS)
	last := SegmentIndex(endUS-1, segUS)
	out := make([]int, 0, last-first+1)
	for i := first; i <= last; i++ {
		out = append(out, i)
	}
	return out
}

// Replay describes one deterministic recalculation of a committed segment.
type Replay struct {
	Rep         model.RepresentationID
	Index       int
	StartUS     int64
	EndUS       int64
	FromVersion int
	ToVersion   int
	Cause       model.PacketID
	CauseTimeUS int64
	Reason      string
}

// SegmentKey identifies one slot of the delivery grid for one representation.
type SegmentKey struct {
	Rep   model.RepresentationID
	Index int
}

// VersionBook tracks the current version of recently committed segments and
// the lifetime counters needed for reporting and checkpoints. Versions for
// segments that can no longer be replayed are forgotten, so memory stays
// bounded by the active window rather than archive length.
type VersionBook struct {
	current  map[SegmentKey]int
	maxVer   int
	replayed int
}

// NewVersionBook returns a book with the lifetime max version at 1.
func NewVersionBook() *VersionBook {
	return &VersionBook{current: map[SegmentKey]int{}, maxVer: 1}
}

// Current returns the latest committed version for a slot, 0 if unknown.
func (b *VersionBook) Current(k SegmentKey) int { return b.current[k] }

// Promote advances a slot to its next version and returns the replay record.
func (b *VersionBook) Promote(rep model.RepresentationID, index int, startUS, endUS int64, cause model.PacketID, causeUS int64) Replay {
	k := SegmentKey{Rep: rep, Index: index}
	from := b.current[k]
	if from == 0 {
		from = 1 // committed before the book started tracking it
	}
	to := from + 1
	b.current[k] = to
	b.replayed++
	if to > b.maxVer {
		b.maxVer = to
	}
	return Replay{
		Rep: rep, Index: index, StartUS: startUS, EndUS: endUS,
		FromVersion: from, ToVersion: to, Cause: cause, CauseTimeUS: causeUS,
		Reason: "late packet inside window recalculates the affected segment",
	}
}

// Record notes an initial commit of a slot at version 1.
func (b *VersionBook) Record(rep model.RepresentationID, index int) {
	k := SegmentKey{Rep: rep, Index: index}
	if b.current[k] == 0 {
		b.current[k] = 1
	}
}

// Forget drops tracked slots strictly older than keepFrom for the given rep.
// The lifetime counters are preserved.
func (b *VersionBook) Forget(rep model.RepresentationID, keepFrom int) {
	for k := range b.current {
		if k.Rep == rep && k.Index < keepFrom {
			delete(b.current, k)
		}
	}
}

// MaxVersion is the highest version ever committed.
func (b *VersionBook) MaxVersion() int { return b.maxVer }

// ReplayedSegments counts replay recalculation events.
func (b *VersionBook) ReplayedSegments() int { return b.replayed }

// Export serialises the book for checkpointing.
func (b *VersionBook) Export() (map[string]int, int, int) {
	m := make(map[string]int, len(b.current))
	for k, v := range b.current {
		m[fmt.Sprintf("%s/%d", k.Rep, k.Index)] = v
	}
	return m, b.maxVer, b.replayed
}

// Import restores a checkpointed book.
func (b *VersionBook) Import(m map[string]int, maxVer, replayed int) error {
	b.current = make(map[SegmentKey]int, len(m))
	for key, v := range m {
		var rep string
		var idx int
		if _, err := fmt.Sscanf(key, "%s", &rep); err != nil {
			return fmt.Errorf("timeline: bad version key %q", key)
		}
		// Split on the final slash: representation ids may contain dashes.
		slash := -1
		for i := len(key) - 1; i >= 0; i-- {
			if key[i] == '/' {
				slash = i
				break
			}
		}
		if slash < 0 {
			return fmt.Errorf("timeline: bad version key %q", key)
		}
		rep = key[:slash]
		if _, err := fmt.Sscanf(key[slash+1:], "%d", &idx); err != nil {
			return fmt.Errorf("timeline: bad version key %q", key)
		}
		b.current[SegmentKey{Rep: model.RepresentationID(rep), Index: idx}] = v
	}
	if maxVer < 1 {
		maxVer = 1
	}
	b.maxVer, b.replayed = maxVer, replayed
	return nil
}
