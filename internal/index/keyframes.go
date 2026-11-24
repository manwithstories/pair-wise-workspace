// Package index builds a keyframe index over a track's sample table and
// answers time queries. Every answer is validated against the actual sample
// before it is returned, and a damaged index is rebuilt from the sample
// table instead of being trusted.
package index

import (
	"fmt"
	"sort"

	"mediarun/internal/clock"
	"mediarun/internal/model"
	"mediarun/internal/parse"
)

// MaxEntries bounds an index so a corrupt table cannot allocate without
// limit.
const MaxEntries = 8192

// Entry is one indexed keyframe.
type Entry struct {
	Number   int        `json:"number"`   // 1-based sample number
	Start    clock.Tick `json:"start"`    // unified tick of the sample
	Duration clock.Tick `json:"duration"` // unified tick duration
	ByteOff  int64      `json:"byte_offset"`
	ByteLen  uint32     `json:"byte_len"`
}

// End returns the exclusive end tick.
func (e Entry) End() (clock.Tick, error) { return clock.Add(e.Start, e.Duration) }

// Index is the on-disk form of a keyframe index.
type Index struct {
	TrackID   uint32  `json:"track_id"`
	Timescale uint32  `json:"timescale"`
	Version   int     `json:"version"`
	Entries   []Entry `json:"entries"`
	Rebuilt   bool    `json:"rebuilt,omitempty"`
}

// CurrentVersion is bumped when the on-disk layout changes; older files are
// rebuilt rather than misread.
const CurrentVersion = 1

// Build derives an index from a parsed track.
func Build(t parse.Track) (*Index, error) {
	if t.Timescale == 0 {
		return nil, fmt.Errorf("%w: track %d timescale", model.ErrInvalid, t.ID)
	}
	sync := t.KeyframeSet()
	idx := &Index{TrackID: t.ID, Timescale: t.Timescale, Version: CurrentVersion}
	_ = sync
	for _, s := range t.Samples {
		if !sync[uint32(s.Index)] {
			continue
		}
		if len(idx.Entries) >= MaxEntries {
			return nil, fmt.Errorf("%w: track %d has more than %d keyframes", model.ErrLimit, t.ID, MaxEntries)
		}
		start, err := clock.ScaleToTicks(int64(s.DTS), t.Timescale)
		if err != nil {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: track %d sample %d time: %v", model.ErrBadLength, t.ID, s.Index, err))
		}
		dur, err := clock.ScaleToTicks(int64(s.Duration), t.Timescale)
		if err != nil {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: track %d sample %d duration: %v", model.ErrBadLength, t.ID, s.Index, err))
		}
		idx.Entries = append(idx.Entries, Entry{
			Number:   s.Index,
			Start:    start,
			Duration: dur,
			ByteOff:  s.Offset,
			ByteLen:  s.Size,
		})
	}
	sort.SliceStable(idx.Entries, func(i, j int) bool {
		if idx.Entries[i].Start != idx.Entries[j].Start {
			return idx.Entries[i].Start < idx.Entries[j].Start
		}
		return idx.Entries[i].Number < idx.Entries[j].Number
	})
	return idx, nil
}

// Validate checks structural invariants. It does not compare against the
// sample table; Lookup does that per query.
func (idx *Index) Validate() error {
	if idx == nil {
		return fmt.Errorf("%w: nil index", model.ErrCorruptIndex)
	}
	if idx.Version != CurrentVersion {
		return fmt.Errorf("%w: version %d", model.ErrCorruptIndex, idx.Version)
	}
	if idx.Timescale == 0 {
		return fmt.Errorf("%w: zero timescale", model.ErrCorruptIndex)
	}
	for i, e := range idx.Entries {
		if e.Number < 1 || e.ByteLen == 0 || e.Duration < 0 {
			return fmt.Errorf("%w: entry %d out of range", model.ErrCorruptIndex, i)
		}
		if _, err := e.End(); err != nil {
			return fmt.Errorf("%w: entry %d end: %v", model.ErrCorruptIndex, i, err)
		}
		if i > 0 {
			prev := idx.Entries[i-1]
			if e.Start < prev.Start || (e.Start == prev.Start && e.Number <= prev.Number) {
				return fmt.Errorf("%w: entry %d not sorted", model.ErrCorruptIndex, i)
			}
		}
	}
	return nil
}

// SampleLookup resolves a sample number to its timing in the track table.
// It is injected so the index package does not need a whole Movie.
type SampleLookup func(number int) (start clock.Tick, dur clock.Tick, offset int64, size uint32, ok bool)

// At returns the keyframe that covers tick t: the last keyframe whose start
// is <= t. Every candidate is verified against the sample table; if the
// index disagrees the index is rejected so the caller can rebuild it.
func (idx *Index) At(t clock.Tick, lookup SampleLookup) (Entry, error) {
	if err := idx.Validate(); err != nil {
		return Entry{}, err
	}
	if len(idx.Entries) == 0 {
		return Entry{}, fmt.Errorf("%w: no keyframes", model.ErrGap)
	}
	i := sort.Search(len(idx.Entries), func(i int) bool { return idx.Entries[i].Start > t })
	if i == 0 {
		// Before the first keyframe there is nothing safe to show.
		return Entry{}, fmt.Errorf("%w: tick %d precedes first keyframe %d", model.ErrGap, t, idx.Entries[0].Start)
	}
	candidate := idx.Entries[i-1]
	if lookup != nil {
		if err := verifyEntry(candidate, lookup); err != nil {
			return Entry{}, err
		}
	}
	return candidate, nil
}

// verifyEntry compares an index entry with the sample table.
func verifyEntry(e Entry, lookup SampleLookup) error {
	start, dur, off, size, ok := lookup(e.Number)
	if !ok {
		return fmt.Errorf("%w: sample %d missing from table", model.ErrCorruptIndex, e.Number)
	}
	if start != e.Start || dur != e.Duration || off != e.ByteOff || size != e.ByteLen {
		return fmt.Errorf("%w: sample %d index (%d,%d,%d,%d) table (%d,%d,%d,%d)",
			model.ErrCorruptIndex, e.Number, e.Start, e.Duration, e.ByteOff, e.ByteLen, start, dur, off, size)
	}
	return nil
}

// Coverage holds the result of a query batch, used to report hit rate.
type Coverage struct {
	Queries int
	Hits    int
	Rebuilt bool
}

// Rate returns the hit rate in [0,1].
func (c Coverage) Rate() float64 {
	if c.Queries == 0 {
		return 0
	}
	return float64(c.Hits) / float64(c.Queries)
}

// Batch queries several ticks. A mismatch triggers exactly one rebuild from
// the sample table, then the batch is retried; the rebuilt index is
// returned for reuse.
func Batch(idx *Index, ticks []clock.Tick, lookup SampleLookup, rebuild func() (*Index, error)) (*Index, Coverage, error) {
	cov := Coverage{Queries: len(ticks)}
	if err := idx.Validate(); err == nil {
		ok := true
		for _, t := range ticks {
			if _, err := idx.At(t, lookup); err != nil {
				ok = false
				break
			}
		}
		if ok {
			cov.Hits = len(ticks)
			return idx, cov, nil
		}
	}
	if rebuild == nil {
		return idx, cov, fmt.Errorf("%w: index unusable and no rebuild available", model.ErrCorruptIndex)
	}
	fresh, err := rebuild()
	if err != nil {
		return idx, cov, err
	}
	if err := fresh.Validate(); err != nil {
		return idx, cov, fmt.Errorf("%w: rebuilt index invalid: %v", model.ErrCorruptIndex, err)
	}
	cov.Rebuilt = true
	for _, t := range ticks {
		if _, err := fresh.At(t, lookup); err == nil {
			cov.Hits++
		}
	}
	return fresh, cov, nil
}
