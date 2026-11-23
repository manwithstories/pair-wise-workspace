package isobmff

import (
	"fmt"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
)

// SampleIndex is the validated per-track view used by downstream nodes.
type SampleIndex struct {
	Track     *Track
	Declared  int64 // samples implied by stts
	Valid     int64 // samples actually addressable (min of stts/stsz)
	Keyframes []int64
	Timeline  *clock.Timeline
	MediaDur  int64 // total media duration from stts (media units)
}

// Index is the validated result of scanning one file.
type Index struct {
	File   *File
	Tracks []*SampleIndex
	Faults fault.List
}

// MediaTimeOf returns the decode time (media units) of a 1-based sample.
func (si *SampleIndex) MediaTimeOf(sample int64) int64 {
	var dts, n int64
	for _, e := range si.Track.Stts {
		for i := uint32(0); i < e.SampleCount; i++ {
			n++
			if n == sample {
				return dts
			}
			dts += int64(e.SampleDelta)
		}
	}
	return dts
}

// SampleAtMediaTime returns the 1-based sample whose decode interval
// contains mt; returns Declared+1 when mt is at/past the end.
func (si *SampleIndex) SampleAtMediaTime(mt int64) int64 {
	var dts, n int64
	for _, e := range si.Track.Stts {
		for i := uint32(0); i < e.SampleCount; i++ {
			n++
			if mt < dts+int64(e.SampleDelta) {
				return n
			}
			dts += int64(e.SampleDelta)
		}
	}
	return n + 1
}

// KeyframePresentationTimes maps sync samples onto the shared time base.
func (si *SampleIndex) KeyframePresentationTimes() ([]clock.Time, error) {
	out := make([]clock.Time, 0, len(si.Keyframes))
	for _, k := range si.Keyframes {
		if k > si.Valid {
			break // keyframes past the recoverable range are unusable
		}
		pt, err := si.Timeline.ToPresentation(si.MediaTimeOf(k))
		if err != nil {
			return nil, err
		}
		out = append(out, pt)
	}
	return out, nil
}

// BuildIndexes validates every track table and locates corruption to the
// first affected sample. It never panics on corrupt input and never reads
// sample data — only the already-parsed tables.
func BuildIndexes(f *File, lim Limits) *Index {
	idx := &Index{File: f}
	if len(f.Tracks) > lim.MaxTracks {
		idx.Faults = append(idx.Faults, &fault.Fault{
			Kind: fault.KindLimitExceeded, Track: 0, Sample: -1, Offset: -1,
			Chain:   []string{"scan"},
			Message: fmt.Sprintf("track count %d exceeds limit %d", len(f.Tracks), lim.MaxTracks),
		})
		return idx
	}
	for _, tr := range f.Tracks {
		si := &SampleIndex{Track: tr}
		idx.Tracks = append(idx.Tracks, si)
		idx.checkTrack(si, lim)
	}
	return idx
}

func (idx *Index) checkTrack(si *SampleIndex, lim Limits) {
	tr := si.Track
	add := func(k fault.Kind, sample, offset int64, recoverable bool, chain []string, msg string) {
		idx.Faults = append(idx.Faults, &fault.Fault{
			Kind: k, Track: tr.Number, Sample: sample, Offset: offset,
			Chain: chain, Recoverable: recoverable, Message: msg,
		})
	}

	// Walk stts once: totals, zero deltas, overflow.
	var declared, mediaDur int64
	for _, e := range tr.Stts {
		if e.SampleDelta == 0 && e.SampleCount > 0 {
			add(fault.KindDuplicateTimestamp, declared+1, tr.OffStts, false,
				[]string{"normalize", "segment", "checksum"},
				fmt.Sprintf("stts entry grants zero duration to %d samples", e.SampleCount))
		}
		d := int64(e.SampleCount) * int64(e.SampleDelta)
		if int64(e.SampleCount) > 0 && d/int64(e.SampleCount) != int64(e.SampleDelta) ||
			mediaDur > (1<<62) || d > (1<<62) {
			add(fault.KindTimestampOverflow, declared+1, tr.OffStts, false,
				[]string{"normalize", "segment", "checksum"}, "cumulative decode time overflows")
			return
		}
		mediaDur += d
		declared += int64(e.SampleCount)
	}
	si.Declared, si.MediaDur = declared, mediaDur

	// Duration limit (24 h) on the shared base, against the larger of the
	// stts-implied and the mdhd-declared media duration.
	effDur := mediaDur
	if tr.MediaDuration > effDur {
		effDur = tr.MediaDuration
	}
	if tr.Timescale > 0 {
		if base, err := clock.ToBase(effDur, tr.Timescale); err != nil {
			add(fault.KindTimestampOverflow, declared, tr.OffStts, false,
				[]string{"normalize", "segment", "checksum"}, err.Error())
			return
		} else if base > lim.MaxDurationSec*clock.BaseTimescale {
			add(fault.KindLimitExceeded, declared, tr.OffStts, false,
				[]string{"scan"},
				fmt.Sprintf("duration %ds exceeds limit %ds", base/clock.BaseTimescale, lim.MaxDurationSec))
		}
	}

	// stsz vs stts: a short stsz means the sample table was truncated.
	stszCount := int64(len(tr.SampleSizes))
	si.Valid = declared
	if stszCount < declared {
		si.Valid = stszCount
		add(fault.KindTruncatedSampleTable, stszCount+1, tr.OffStsz, true,
			[]string{"normalize", "segment", "checksum"},
			fmt.Sprintf("stsz declares %d samples, stts implies %d", stszCount, declared))
	}

	// stco offsets must stay inside the file.
	for i, off := range tr.ChunkOffsets {
		if int64(off) >= idx.File.Size {
			sample := int64(1)
			for _, e := range tr.Stsc {
				if int64(e.FirstChunk)-1 <= int64(i) {
					sample = int64(e.SamplesPerChunk)*int64(i) + 1
				}
			}
			add(fault.KindOffsetOutOfRange, sample, tr.OffStco, false,
				[]string{"segment", "checksum"},
				fmt.Sprintf("chunk %d offset %d beyond file size %d", i+1, off, idx.File.Size))
		}
	}

	// Keyframes: stss, or every sample when no stss exists.
	if len(tr.SyncSamples) > 0 {
		for _, n := range tr.SyncSamples {
			si.Keyframes = append(si.Keyframes, int64(n))
		}
	} else {
		for n := int64(1); n <= declared; n++ {
			si.Keyframes = append(si.Keyframes, n)
		}
	}

	// Edit list: structural validation, then range against media duration.
	tl, err := clock.NewTimeline(tr.Timescale, idx.File.MovieTimescale, tr.Edits)
	if err != nil {
		add(fault.KindEditListOverlap, -1, tr.OffElst, false,
			[]string{"normalize", "segment", "checksum"}, err.Error())
		return
	}
	si.Timeline = tl
	for _, e := range tr.Edits {
		if e.Empty {
			continue
		}
		end := e.MediaTime + e.Duration*tr.Timescale/idx.File.MovieTimescale
		if e.MediaTime > mediaDur || end > mediaDur {
			add(fault.KindEditListOutOfRange, si.SampleAtMediaTime(e.MediaTime), tr.OffElst, false,
				[]string{"normalize", "segment", "checksum"},
				fmt.Sprintf("edit media range [%d,%d) exceeds media duration %d",
					e.MediaTime, end, mediaDur))
		}
	}
}
