// Package index builds a per-frame table (dts/pts/duration/offset/
// keyframe) from a parsed sample table and locates frames by time.
package index

import (
	"errors"
	"sort"

	"mp4tool/internal/box"
	"mp4tool/internal/timeline"
)

var (
	// ErrSampleOutOfRange reports a sample offset beyond the file.
	ErrSampleOutOfRange = errors.New("sample_out_of_range")
	// ErrSeekOutOfRange reports a seek target with no landing frame.
	ErrSeekOutOfRange = errors.New("seek_out_of_range")
	// ErrTableInconsistent reports mismatched table entry counts.
	ErrTableInconsistent = errors.New("table_inconsistent")
)

// Frame describes one sample on the decode and presentation timelines.
type Frame struct {
	Index     int    // decode-order sample index (0-based)
	DTS       int64  // media ticks
	PTS       int64  // raw media ticks (dts + ctts)
	NormPTS   int64  // presentation ticks after edit list / wrap restore
	Duration  int64  // media ticks
	Offset    uint64 // absolute file offset
	Size      uint32
	Keyframe  bool
	Presented bool // inside an edit-list segment
}

// Build expands stts/ctts/stss/stsz/stsc/stco into a frame table.
// wrapEvents counts ctts v0 offsets restored from 32-bit unsigned
// miswrites (>= 2^31 treated as negative).
func Build(t *box.Trak, m *timeline.Mapper, fileSize uint64) ([]Frame, int, error) {
	st := &t.Table
	n := len(st.Sizes)
	if n == 0 {
		return nil, 0, ErrTableInconsistent
	}
	dts := make([]int64, n)
	dur := make([]int64, n)
	i, acc := 0, int64(0)
	for _, e := range st.Stts {
		for k := uint32(0); k < e.Count; k++ {
			if i >= n {
				return nil, 0, ErrTableInconsistent
			}
			dts[i], dur[i] = acc, int64(e.Delta)
			acc += int64(e.Delta)
			i++
		}
	}
	if i != n {
		return nil, 0, ErrTableInconsistent
	}
	ctts := make([]int64, n)
	wrapEvents := 0
	i = 0
	for _, e := range st.Ctts {
		off := e.Offset
		if off >= 1<<31 { // v0 unsigned miswrite of a wrapped offset
			off -= 1 << 32
			wrapEvents++
		}
		for k := uint32(0); k < e.Count; k++ {
			if i >= n {
				return nil, 0, ErrTableInconsistent
			}
			ctts[i] = off
			i++
		}
	}
	if len(st.Ctts) > 0 && i != n {
		return nil, 0, ErrTableInconsistent
	}
	key := make([]bool, n)
	if len(st.Stss) == 0 {
		for j := range key {
			key[j] = true
		}
	} else {
		for _, s := range st.Stss {
			if s == 0 || int(s) > n {
				return nil, 0, ErrTableInconsistent
			}
			key[s-1] = true
		}
	}
	offsets, err := sampleOffsets(st, n, fileSize)
	if err != nil {
		return nil, 0, err
	}
	frames := make([]Frame, n)
	for j := 0; j < n; j++ {
		pts := dts[j] + ctts[j]
		norm, presented := m.Map(pts)
		frames[j] = Frame{
			Index: j, DTS: dts[j], PTS: pts, NormPTS: norm,
			Duration: dur[j], Offset: offsets[j], Size: st.Sizes[j],
			Keyframe: key[j], Presented: presented,
		}
	}
	return frames, wrapEvents, nil
}

// sampleOffsets resolves stsc/stco/stsz into absolute sample offsets,
// handling stsc boundaries and samples spanning chunk transitions.
func sampleOffsets(st *box.SampleTable, n int, fileSize uint64) ([]uint64, error) {
	if len(st.Stsc) == 0 || len(st.Chunks) == 0 {
		return nil, ErrTableInconsistent
	}
	offsets := make([]uint64, 0, n)
	entry := 0
	for chunk := 1; chunk <= len(st.Chunks); chunk++ {
		for entry+1 < len(st.Stsc) && int(st.Stsc[entry+1].FirstChunk) <= chunk {
			entry++
		}
		per := int(st.Stsc[entry].SamplesPerChunk)
		off := st.Chunks[chunk-1]
		for k := 0; k < per && len(offsets) < n; k++ {
			size := uint64(st.Sizes[len(offsets)])
			if off+size > fileSize {
				return nil, ErrSampleOutOfRange
			}
			offsets = append(offsets, off)
			off += size
		}
	}
	if len(offsets) != n {
		return nil, ErrTableInconsistent
	}
	return offsets, nil
}

// Locate finds the frame to land on for a presentation-time target
// (media ticks). Non-exact seeks land on the nearest keyframe not
// after the target; exact seeks may land on any frame.
func Locate(frames []Frame, targetTicks int64, exact bool) (Frame, error) {
	cand := make([]Frame, 0, len(frames))
	for _, f := range frames {
		if !f.Presented || (!exact && !f.Keyframe) {
			continue
		}
		cand = append(cand, f)
	}
	sort.Slice(cand, func(a, b int) bool { return cand[a].NormPTS < cand[b].NormPTS })
	pos := sort.Search(len(cand), func(i int) bool { return cand[i].NormPTS > targetTicks }) - 1
	if pos < 0 {
		return Frame{}, ErrSeekOutOfRange
	}
	return cand[pos], nil
}
