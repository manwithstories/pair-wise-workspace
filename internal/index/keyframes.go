// Package index builds and verifies keyframe indexes on the unified
// time base, from either ingested frames or a parsed MP4 sample table.
package index

import "media-check/internal/model"

// Index maps keyframe ordinal → frame position and unified time.
type Index struct {
	Positions []int
	Times     []int64
}

// Build derives the index from ingested frames.
func Build(frames []model.Frame) Index {
	var idx Index
	for i, f := range frames {
		if f.Keyframe {
			idx.Positions = append(idx.Positions, i)
			idx.Times = append(idx.Times, f.Time)
		}
	}
	return idx
}

// FromSampleTable derives the index from an MP4 stss table. keySamples
// are 1-based sample numbers; delta is the constant sample duration.
func FromSampleTable(keySamples []uint32, delta uint32, startTime int64) Index {
	var idx Index
	for _, s := range keySamples {
		if s == 0 {
			continue
		}
		pos := int(s - 1)
		idx.Positions = append(idx.Positions, pos)
		idx.Times = append(idx.Times, startTime+int64(pos)*int64(delta))
	}
	return idx
}

// Verify checks that every keyframe in frames is present in the index
// with a matching unified time. It returns hits and total keyframes.
func Verify(idx Index, frames []model.Frame) (hits, total int) {
	byPos := map[int]int64{}
	for i, p := range idx.Positions {
		byPos[p] = idx.Times[i]
	}
	for i, f := range frames {
		if !f.Keyframe {
			continue
		}
		total++
		if t, ok := byPos[i]; ok && t == f.Time {
			hits++
		}
	}
	return hits, total
}
