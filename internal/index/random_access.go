// Package index builds and queries deterministic random-access indexes over
// repaired media segments.
package index

import (
	"fmt"
	"sort"

	"github.com/pairwise/mediad/internal/media"
)

// Entry locates one independently addressable segment or keyframe.
type Entry struct {
	Index       int    `json:"index"`
	SegmentID   string `json:"segment_id"`
	Path        string `json:"path"`
	StartTicks  int64  `json:"start_ticks"`
	EndTicks    int64  `json:"end_ticks"`
	Keyframe    bool   `json:"keyframe"`
	BoxOffset   int64  `json:"box_offset"`
	Size        int    `json:"size"`
	SHA256      string `json:"sha256"`
	Complete    bool   `json:"complete"`
}

// Index is a sorted random-access table.
type Index struct {
	Base    media.Rational `json:"base"`
	Entries []Entry        `json:"entries"`
}

// New builds and validates a sorted index. It never marks an incomplete entry
// as keyframe-capable.
func New(entries []Entry) (*Index, error) {
	cp := append([]Entry(nil), entries...)
	sort.SliceStable(cp, func(i, j int) bool {
		if cp[i].StartTicks != cp[j].StartTicks {
			return cp[i].StartTicks < cp[j].StartTicks
		}
		return cp[i].Index < cp[j].Index
	})
	for i := range cp {
		if cp[i].StartTicks < 0 || cp[i].EndTicks <= cp[i].StartTicks {
			return nil, media.NewError(media.CodeMalformed, "index.random_access", fmt.Sprintf("entries[%d]", i), "invalid time interval")
		}
		if cp[i].Keyframe && (!cp[i].Complete || cp[i].Size == 0) {
			return nil, media.NewError(media.CodeMalformed, "index.random_access", cp[i].SegmentID, "keyframe requires complete non-empty segment")
		}
		if i > 0 && cp[i].StartTicks < cp[i-1].StartTicks {
			return nil, media.NewError(media.CodeMalformed, "index.random_access", "StartTicks", "index is not monotonic")
		}
	}
	return &Index{Base: media.CommonTimebase, Entries: cp}, nil
}

// TimeRange returns first and last tick covered.
func (idx *Index) TimeRange() (int64, int64) {
	if len(idx.Entries) == 0 {
		return 0, 0
	}
	return idx.Entries[0].StartTicks, idx.Entries[len(idx.Entries)-1].EndTicks
}

// AtTime returns the segment covering t. Queries at EndTicks advance to the
// next segment unless t is the final end.
func (idx *Index) AtTime(t int64) (Entry, error) {
	if len(idx.Entries) == 0 {
		return Entry{}, media.NewError(media.CodeOutOfRange, "index.random_access", "time", "empty index")
	}
	minT, maxT := idx.TimeRange()
	if t < minT || t > maxT {
		return Entry{}, media.NewError(media.CodeOutOfRange, "index.random_access", "time",
			fmt.Sprintf("ticks=%d outside [%d,%d]", t, minT, maxT))
	}
	i := sort.Search(len(idx.Entries), func(i int) bool { return idx.Entries[i].EndTicks > t })
	if i == len(idx.Entries) {
		i = len(idx.Entries) - 1
	}
	e := idx.Entries[i]
	if t < e.StartTicks || t > e.EndTicks {
		return Entry{}, media.NewError(media.CodeOutOfRange, "index.random_access", "time", "fallback query missed segment")
	}
	return e, nil
}

// Segment returns an entry by its zero-based segment number.
func (idx *Index) Segment(segmentIndex int) (Entry, error) {
	for _, e := range idx.Entries {
		if e.Index == segmentIndex {
			return e, nil
		}
	}
	return Entry{}, media.NewError(media.CodeOutOfRange, "index.random_access", "segment_index",
		fmt.Sprintf("segment %d not found (count=%d)", segmentIndex, len(idx.Entries)))
}

// SeekKeyframe returns the nearest complete keyframe at or before t.
func (idx *Index) SeekKeyframe(t int64) (Entry, error) {
	candidate := -1
	for i, e := range idx.Entries {
		if e.Keyframe && e.Complete && e.StartTicks <= t {
			candidate = i
		}
	}
	if candidate < 0 {
		return Entry{}, media.NewError(media.CodeOutOfRange, "index.random_access", "keyframe",
			fmt.Sprintf("no complete keyframe at or before ticks=%d", t))
	}
	return idx.Entries[candidate], nil
}

// KeyframeHitRate runs deterministic random-access queries and reports how many
// resolve to a keyframe. A miss is returned as an error by SeekKeyframe.
func (idx *Index) KeyframeHitRate(queryTicks []int64) (hits, total int, rate float64, err error) {
	total = len(queryTicks)
	for _, t := range queryTicks {
		e, seekErr := idx.SeekKeyframe(t)
		if seekErr != nil {
			return hits, total, float64(hits) * 100 / float64(max(1, total)), seekErr
		}
		if e.Keyframe {
			hits++
		}
	}
	if total == 0 {
		return 0, 0, 100, nil
	}
	return hits, total, float64(hits) * 100 / float64(total), nil
}
