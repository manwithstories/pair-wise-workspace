// Package index rebuilds a random-access index from sample tables and
// verifies that seeks land on real samples in decode order.
package index

import (
	"fmt"
	"sort"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// Entry is one seek point. An entry always names a real sync sample: the
// index never points at a sample that was not delivered.
type Entry struct {
	// SampleIndex is the position of the sync sample in decode order.
	SampleIndex int `json:"sample_index"`
	// Time is the presentation timestamp of the sync sample.
	Time clock.Rational `json:"time"`
	// DTS is the decode timestamp, retained so a seek can verify the index
	// against decode order.
	DTS clock.Rational `json:"dts"`
	// Offset and Size locate the sample payload.
	Offset int64  `json:"offset"`
	Size   uint32 `json:"size"`
	// Damaged marks a seek point whose payload is incomplete. Such entries
	// are retained but flagged so a player can pick a neighbour instead.
	Damaged bool `json:"damaged"`
}

// TrackIndex is the seek index for one track.
type TrackIndex struct {
	TrackID uint32 `json:"track_id"`
	// Timescale is the denominator used for reported ticks.
	Timescale uint32  `json:"timescale"`
	Entries   []Entry `json:"entries"`
	// Skipped counts sync samples excluded because they were damaged or
	// synthesised; they are reported rather than silently indexed.
	Skipped int `json:"skipped"`
}

// SeekResult is the outcome of a sample seek.
type SeekResult struct {
	// Target is the requested time.
	Target clock.Rational `json:"target"`
	// Entry is the chosen sync sample.
	Entry Entry `json:"entry"`
	// Mismatch reports why a seek was rejected, if it was.
	Mismatch string `json:"mismatch,omitempty"`
}

// Rebuild builds the index for a repaired track.
func Rebuild(tr model.Track) (TrackIndex, error) {
	idx := TrackIndex{TrackID: tr.ID}
	if len(tr.Samples) == 0 {
		return idx, model.New(model.KindInvalidInput, "index.rebuild", "track has no samples").WithTrack(tr.ID)
	}
	idx.Timescale = tr.Timescale
	if idx.Timescale == 0 {
		idx.Timescale = 1
	}
	prev := clock.Rational{}
	for i, s := range tr.Samples {
		if i > 0 && !prev.Less(s.DTS) {
			return idx, model.New(model.KindInvalidMedia, "index.rebuild",
				fmt.Sprintf("sample %d DTS %v is not after %v", i, s.DTS, prev)).WithTrack(tr.ID)
		}
		prev = s.DTS
		if !s.Keyframe {
			continue
		}
		if s.Damaged || s.Synthesised {
			idx.Skipped++
			continue
		}
		idx.Entries = append(idx.Entries, Entry{
			SampleIndex: i, Time: s.PTS, DTS: s.DTS,
			Offset: s.Offset, Size: s.Size,
		})
	}
	if len(idx.Entries) == 0 {
		return idx, model.New(model.KindInvalidMedia, "index.rebuild",
			"no intact sync samples to index").WithTrack(tr.ID)
	}
	sort.SliceStable(idx.Entries, func(a, b int) bool {
		return idx.Entries[a].Time.Less(idx.Entries[b].Time)
	})
	return idx, nil
}

// expectedEntry computes, directly from the track, the sync sample a seek to
// target must start from. It is the independent oracle Verify compares
// against.
func expectedEntry(tr model.Track, target clock.Rational) Entry {
	best := Entry{SampleIndex: -1}
	for i, s := range tr.Samples {
		if !s.Keyframe || s.Damaged || s.Synthesised {
			continue
		}
		if s.PTS.Less(target) || s.PTS.Equal(target) {
			if best.SampleIndex < 0 || best.Time.Less(s.PTS) || best.Time.Equal(s.PTS) {
				best = Entry{SampleIndex: i, Time: s.PTS, DTS: s.DTS, Offset: s.Offset, Size: s.Size}
			}
			continue
		}
		// Samples are in decode order; once a sync sample is past target a
		// later one cannot be nearer.
		break
	}
	return best
}

// Seek finds the sync sample that starts at or before target. It returns an
// error when target precedes the first sync sample or lies past the last
// one's valid range.
func (idx TrackIndex) Seek(target clock.Rational, lastEnd clock.Rational) (SeekResult, error) {
	res := SeekResult{Target: target}
	if len(idx.Entries) == 0 {
		return res, model.New(model.KindInvalidInput, "index.seek", "empty index").WithTrack(idx.TrackID)
	}
	if target.Less(idx.Entries[0].Time) {
		return res, model.New(model.KindInvalidMedia, "index.seek",
			fmt.Sprintf("target %v precedes first sync sample %v", target, idx.Entries[0].Time)).WithTrack(idx.TrackID)
	}
	if lastEnd.Sign() > 0 && lastEnd.Less(target) {
		return res, model.New(model.KindInvalidMedia, "index.seek",
			fmt.Sprintf("target %v is past track end %v", target, lastEnd)).WithTrack(idx.TrackID)
	}
	pos := sort.Search(len(idx.Entries), func(i int) bool {
		return !idx.Entries[i].Time.Less(target)
	})
	if pos < len(idx.Entries) && idx.Entries[pos].Time.Equal(target) {
		res.Entry = idx.Entries[pos]
		return res, nil
	}
	// Otherwise the entry just before target is where decoding must start.
	pos--
	if pos < 0 {
		pos = 0
	}
	res.Entry = idx.Entries[pos]
	return res, nil
}

// Verify samples seeks across the index and checks that each result lands on
// a genuine sync sample and that seek order agrees with decode order. It
// returns the number of mismatches and a description of each.
func Verify(tr model.Track, idx TrackIndex, seeks int) (int, []string, error) {
	if seeks <= 0 {
		return 0, nil, nil
	}
	if len(tr.Samples) == 0 || len(idx.Entries) == 0 {
		return 0, nil, model.New(model.KindInvalidInput, "index.verify", "nothing to verify").WithTrack(tr.ID)
	}
	end, _ := tr.End()
	first := idx.Entries[0].Time
	last := idx.Entries[len(idx.Entries)-1].Time
	if !first.Less(last) {
		return 0, nil, model.New(model.KindInvalidMedia, "index.verify", "index spans no time").WithTrack(tr.ID)
	}

	span, err := last.Sub(first)
	if err != nil {
		return 0, nil, err
	}
	if seeks == 1 {
		seeks = 2 // always probe both ends
	}

	var mismatches []string
	lastSample := -1
	lastTime := clock.Rational{}
	for i := 0; i < seeks; i++ {
		// Deterministic even spread from the first sync sample to the last,
		// so the probes themselves are in non-decreasing target order.
		f, err := span.Mul(clock.MustNew(int64(i), int64(seeks-1)))
		if err != nil {
			return 0, nil, err
		}
		target, err := first.Add(f)
		if err != nil {
			return 0, nil, err
		}
		res, err := idx.Seek(target, end)
		if err != nil {
			mismatches = append(mismatches, fmt.Sprintf("seek %v: %v", target, err))
			continue
		}
		e := res.Entry
		if e.SampleIndex < 0 || e.SampleIndex >= len(tr.Samples) {
			mismatches = append(mismatches, fmt.Sprintf("seek %v: sample index %d out of range", target, e.SampleIndex))
			continue
		}
		// Every index entry must describe a real, intact sync sample. Entries
		// are matched by sample index, so a tampered timestamp is caught even
		// when the seek itself succeeds.
		s := tr.Samples[e.SampleIndex]
		if !s.Keyframe || s.Damaged || s.Synthesised {
			mismatches = append(mismatches, fmt.Sprintf("seek %v: entry %d is not a usable sync sample", target, e.SampleIndex))
		}
		if s.DTS != e.DTS || s.PTS != e.Time || s.Offset != e.Offset {
			mismatches = append(mismatches, fmt.Sprintf("seek %v: index entry disagrees with sample %d", target, e.SampleIndex))
		}
		// Independently re-derive the expected entry from the track's own
		// timestamps and compare.
		want := expectedEntry(tr, target)
		if want.SampleIndex != e.SampleIndex {
			mismatches = append(mismatches, fmt.Sprintf("seek %v: index chose sample %d, decode order requires %d",
				target, e.SampleIndex, want.SampleIndex))
		}
		// Non-decreasing target order must produce non-decreasing decode
		// order, otherwise the index would send a decoder backwards.
		if lastSample >= 0 && e.SampleIndex < lastSample {
			mismatches = append(mismatches, fmt.Sprintf("seek %v: decode order went backwards from %d to %d", target, lastSample, e.SampleIndex))
		}
		if lastSample >= 0 && e.Time.Less(lastTime) {
			mismatches = append(mismatches, fmt.Sprintf("seek %v: presentation time went backwards", target))
		}
		lastSample = e.SampleIndex
		lastTime = e.Time
	}
	return len(mismatches), mismatches, nil
}
