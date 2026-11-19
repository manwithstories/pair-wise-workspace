// Package index builds and validates keyframe indices for a media track.
// An index maps keyframe sample numbers (1-based, matching the ISOBMFF stss
// convention) to their byte offsets inside the container payload. Every entry
// is validated against the real sample range and payload span, so an
// out-of-range or bogus index is reported rather than producing a false
// keyframe hit.
package index

import (
	"mediapipeline/internal/mediaerr"
)

// Entry is one validated keyframe reference.
type Entry struct {
	// SampleNumber is 1-based and must be within [1, SampleCount].
	SampleNumber uint32
	// Offset is the keyframe's byte position inside the payload region.
	Offset uint64
}

// Sample describes one media sample for index validation.
type Sample struct {
	// IsKeyframe is taken from the packet marker / frame-type byte only;
	// the builder never infers a keyframe from position or timing.
	IsKeyframe bool
	// Size is the sample's byte length inside the container payload.
	Size int
}

// Index is the validated keyframe index for one track.
type Index struct {
	track       uint32
	sampleCount uint32
	payloadSize uint64
	entries     []Entry
}

// Build constructs an index from the sample manifest. It returns a locatable
// error when no keyframe exists; the returned empty Index is still usable for
// reporting (callers decide whether absence is fatal for their track type).
func Build(track uint32, samples []Sample) (*Index, *mediaerr.Error) {
	idx := &Index{track: track, sampleCount: uint32(len(samples))}
	var offset uint64
	for i, s := range samples {
		if s.Size < 0 {
			return nil, mediaerr.New(mediaerr.StageIndex, mediaerr.KindInvalidInput,
				"negative sample size").WithTrack(track).
				WithSeq(uint16(i + 1))
		}
		if s.IsKeyframe {
			idx.entries = append(idx.entries, Entry{
				SampleNumber: uint32(i + 1),
				Offset:       offset,
			})
		}
		offset += uint64(s.Size)
	}
	idx.payloadSize = offset
	if len(idx.entries) == 0 {
		return idx, mediaerr.New(mediaerr.StageIndex, mediaerr.KindNoKeyframe,
			"track contains no keyframe samples").WithTrack(track)
	}
	return idx, nil
}

// Validate checks externally supplied entries against the sample range and
// payload span. It returns the first out-of-range entry as a structured error
// so corrupted or fabricated indices cannot create false keyframe hits.
func (x *Index) Validate(entries []Entry) *mediaerr.Error {
	for _, e := range entries {
		if e.SampleNumber == 0 || e.SampleNumber > x.sampleCount {
			return mediaerr.New(mediaerr.StageIndex, mediaerr.KindIndexOutOfRange,
				"keyframe sample number outside sample range").
				WithTrack(x.track).WithSeq(uint16(min32(e.SampleNumber, 0xFFFF)))
		}
		if e.Offset >= x.payloadSize {
			return mediaerr.New(mediaerr.StageIndex, mediaerr.KindIndexOutOfRange,
				"keyframe offset outside payload region").
				WithTrack(x.track).WithOffset(int64(e.Offset))
		}
	}
	return nil
}

// Entries returns the validated keyframe entries.
func (x *Index) Entries() []Entry { return x.entries }

// SampleCount returns the total number of samples.
func (x *Index) SampleCount() uint32 { return x.sampleCount }

// PayloadSize returns the summed sample byte size.
func (x *Index) PayloadSize() uint64 { return x.payloadSize }

// HitRate reports the fraction of queried keyframe sample numbers that the
// index actually contains. A fabricated query (not among the entries) is a
// miss, so the rate can never be inflated by bogus input.
func (x *Index) HitRate(querySampleNumbers []uint32) (hits int, rate float64) {
	if len(querySampleNumbers) == 0 {
		return 0, 0
	}
	known := make(map[uint32]struct{}, len(x.entries))
	for _, e := range x.entries {
		known[e.SampleNumber] = struct{}{}
	}
	for _, q := range querySampleNumbers {
		if _, ok := known[q]; ok {
			hits++
		}
	}
	return hits, float64(hits) / float64(len(querySampleNumbers))
}

func min32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}
