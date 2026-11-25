// Package timeline maps media timestamps to a presentation timeline
// through an explicit edit list, handling non-zero starts and gaps.
package timeline

import "fmt"

// Edit maps [MediaStart, MediaStart+Duration) of the media timeline to
// the next slot on the presentation timeline. Ticks are track timescale.
type Edit struct {
	MediaStart int64
	Duration   int64
}

// Mapper translates between media and presentation ticks.
type Mapper struct {
	edits     []Edit
	presStart []int64 // presentation offset where each edit begins
	total     int64
}

// NewMapper validates the edit list and precomputes presentation offsets.
// An empty edit list is the identity over the whole media timeline.
func NewMapper(edits []Edit) (*Mapper, error) {
	if len(edits) == 0 {
		return &Mapper{}, nil
	}
	m := &Mapper{edits: append([]Edit(nil), edits...)}
	var acc int64
	for i, e := range edits {
		if e.MediaStart < 0 {
			return nil, fmt.Errorf("edit %d: negative media_start %d", i, e.MediaStart)
		}
		if e.Duration <= 0 {
			return nil, fmt.Errorf("edit %d: non-positive duration %d", i, e.Duration)
		}
		m.presStart = append(m.presStart, acc)
		acc += e.Duration
	}
	m.total = acc
	return m, nil
}

// Total returns the total presentation duration in ticks. Zero when the
// mapper is the identity (no explicit edits).
func (m *Mapper) Total() int64 { return m.total }

// MediaToPres maps a media timestamp to presentation ticks. The second
// result is false when the time falls outside every edit (a gap).
func (m *Mapper) MediaToPres(mt int64) (int64, bool) {
	if len(m.edits) == 0 {
		return mt, true
	}
	for i, e := range m.edits {
		if mt >= e.MediaStart && mt < e.MediaStart+e.Duration {
			return m.presStart[i] + (mt - e.MediaStart), true
		}
	}
	return 0, false
}

// PresToMedia maps a presentation timestamp back to media ticks.
func (m *Mapper) PresToMedia(pt int64) (int64, bool) {
	if len(m.edits) == 0 {
		return pt, true
	}
	if pt < 0 || pt >= m.total {
		return 0, false
	}
	for i := len(m.edits) - 1; i >= 0; i-- {
		if pt >= m.presStart[i] {
			return m.edits[i].MediaStart + (pt - m.presStart[i]), true
		}
	}
	return 0, false
}
