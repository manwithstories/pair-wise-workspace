// Package segment aligns rendition frames at common IDR boundaries and emits
// deterministic CMAF segments.
package segment

import (
	"sort"

	"mediad/internal/codec"
	"mediad/internal/mediaerr"
	"mediad/internal/timeline"
)

// BoundaryTolerance90k is 20 ms in 90 kHz MPEG timestamp units.
const BoundaryTolerance90k = int64(20 * timeline.PTSHz / 1000) // 1800

// MaxFrames bounds per-rendition buffering.
const MaxFrames = 8192

// Frame is one parsed elementary frame on a common unwrapped time base.
type Frame struct {
	Rendition int
	PID       uint16
	Offset    int64
	PTS       int64
	DTS       int64
	Keyframe  bool
	Codec     codec.Codec
	Payload   []byte
}

// Track is one rendition's bounded frame collection.
type Track struct {
	ID     string
	Frames []Frame
}

// Boundary is a matched common IDR cut.
type Boundary struct {
	Index        int
	PTS          int64
	RenditionPTS []int64
	SkewMS       float64
}

// RenditionChunk contains one rendition's frames in an aligned segment.
type RenditionChunk struct {
	Rendition int
	StartPTS  int64
	EndPTS    int64
	Frames    []Frame
}

// Segment is a safe, independently decodable aligned interval.
type Segment struct {
	Index      int
	StartPTS   int64
	EndPTS     int64
	DurationMS int64
	MaxSkewMS  float64
	Chunks     []RenditionChunk
	Boundary   Boundary
}

// Alignment is the result of common-IDR alignment.
type Alignment struct {
	Boundaries []Boundary
	Segments   []Segment
	MaxSkewMS  float64
}

// AlignTracks matches keyframes and builds safe segments. If no common
// keyframe exists, it returns KindNoCommonKeyframe. A later common key is used
// when an earlier desired boundary cannot be matched.
func AlignTracks(tracks []Track) (*Alignment, error) {
	if len(tracks) == 0 {
		return nil, mediaerr.New(mediaerr.KindInvalidArgument, -1, 0, false, "no renditions to align")
	}
	for i := range tracks {
		if len(tracks[i].Frames) > MaxFrames {
			return nil, mediaerr.New(mediaerr.KindBufferLimit, -1, 0, false,
				"rendition %d has %d frames > bound %d", i, len(tracks[i].Frames), MaxFrames)
		}
		sort.SliceStable(tracks[i].Frames, func(a, b int) bool {
			return tracks[i].Frames[a].PTS < tracks[i].Frames[b].PTS
		})
	}

	keys := make([][]int64, len(tracks))
	for r, tr := range tracks {
		for _, f := range tr.Frames {
			if f.Keyframe && f.Codec == codec.CodecH264 {
				keys[r] = append(keys[r], f.PTS)
			}
		}
		if len(keys[r]) == 0 {
			return nil, mediaerr.New(mediaerr.KindNoCommonKeyframe, -1, 0, false,
				"rendition %d has no IDR keyframes", r)
		}
	}

	bounds, err := matchBoundaries(keys)
	if err != nil {
		return nil, err
	}
	out := &Alignment{Boundaries: bounds}
	// Only intervals closed by a following common IDR are committed. This
	// avoids inventing a VFR frame duration for the trailing partial interval.
	for i := 0; i+1 < len(bounds); i++ {
		start := bounds[i].PTS
		end := bounds[i+1].PTS
		seg := Segment{
			Index: i, StartPTS: start, EndPTS: end,
			DurationMS: int64(timeline.PTSDurationMS(end - start)),
			MaxSkewMS:  bounds[i].SkewMS, Boundary: bounds[i],
		}
		for r, tr := range tracks {
			rs := bounds[i].RenditionPTS[r]
			re := bounds[i+1].RenditionPTS[r]
			chunk := RenditionChunk{Rendition: r, StartPTS: rs, EndPTS: re}
			for _, f := range tr.Frames {
				if f.PTS >= rs && f.PTS < re {
					chunk.Frames = append(chunk.Frames, f)
				}
			}
			if len(chunk.Frames) == 0 {
				return nil, mediaerr.New(mediaerr.KindNoCommonKeyframe, -1, 0, false,
					"aligned segment %d has no frames for rendition %d", i, r)
			}
			seg.Chunks = append(seg.Chunks, chunk)
		}
		if bounds[i].SkewMS > out.MaxSkewMS {
			out.MaxSkewMS = bounds[i].SkewMS
		}
		out.Segments = append(out.Segments, seg)
	}
	if len(out.Segments) == 0 {
		return nil, mediaerr.New(mediaerr.KindNoCommonKeyframe, -1, 0, false,
			"need at least two common IDR boundaries to close a safe segment")
	}
	return out, nil
}
func matchBoundaries(keys [][]int64) ([]Boundary, error) {
	ref := keys[0]
	used := make([][]bool, len(keys)-1)
	for i := range used {
		used[i] = make([]bool, len(keys[i+1]))
	}
	var bounds []Boundary
	for _, p := range ref {
		rpts := make([]int64, len(keys))
		rpts[0] = p
		maxSkew := 0.0
		ok := true
		for r := 1; r < len(keys); r++ {
			match := -1
			best := BoundaryTolerance90k + 1
			for j, q := range keys[r] {
				if used[r-1][j] {
					continue
				}
				d := abs64(q - p)
				if d <= BoundaryTolerance90k && d < best {
					best, match = d, j
				}
			}
			if match < 0 {
				ok = false
				break
			}
			used[r-1][match] = true
			rpts[r] = keys[r][match]
			if sk := timeline.BoundarySkewMS(p, rpts[r]); sk > maxSkew {
				maxSkew = sk
			}
		}
		if ok {
			bounds = append(bounds, Boundary{Index: len(bounds), PTS: p, RenditionPTS: rpts, SkewMS: maxSkew})
		}
	}
	if len(bounds) == 0 {
		return nil, mediaerr.New(mediaerr.KindNoCommonKeyframe, -1, 0, false,
			"no IDR boundaries within 20ms tolerance")
	}
	return bounds, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
