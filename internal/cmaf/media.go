package cmaf

import (
	"fmt"

	"github.com/abema/go-mp4"

	"mediaflow/internal/clock"
)

// Scanner holds the per-representation decoder state. It is deliberately not
// concurrent-safe: one Scanner belongs to one representation and the fan-out
// keeps representations independent.
type Scanner struct {
	Limits Limits
	Tracks map[uint32]TrackInfo
	Order  []uint32
	// Defaults tracks trex defaults learned from the init segment.
	Defaults map[uint32]mp4.Trex
}

// NewScanner returns a scanner with the given limits.
func NewScanner(limits Limits) *Scanner {
	return &Scanner{Limits: limits, Tracks: make(map[uint32]TrackInfo), Defaults: make(map[uint32]mp4.Trex)}
}

// ApplyInit loads track metadata from a parsed initialization segment.
func (s *Scanner) ApplyInit(init InitSegment) {
	for _, t := range init.Tracks {
		s.Tracks[t.TrackID] = t
		if _, ok := s.Defaults[t.TrackID]; !ok {
			s.Defaults[t.TrackID] = mp4.Trex{
				TrackID:               t.TrackID,
				DefaultSampleDuration: t.DefaultSampleDuration,
				DefaultSampleSize:     t.DefaultSampleSize,
				DefaultSampleFlags:    t.DefaultSampleFlags,
			}
		}
		s.Order = append(s.Order, t.TrackID)
	}
}

// MediaSegment is the parsed form of one CMAF media segment.
type MediaSegment struct {
	SegmentIndex int                  `json:"segment_index"`
	Size         int                  `json:"size"`
	Fragments    []Fragment           `json:"fragments"`
	Problems     []clock.LocatedError `json:"problems,omitempty"`
	MoofCount    int                  `json:"moof_count"`
	MdatBytes    uint64               `json:"mdat_bytes"`
}

// ParseMedia incrementally parses one media segment. Every moof is decoded
// within its own bounds; a malformed moof is recorded and parsing resumes at
// the next top-level anchor.
func (s *Scanner) ParseMedia(buf []byte, segIdx int) (MediaSegment, error) {
	out := MediaSegment{SegmentIndex: segIdx, Size: len(buf)}
	if len(buf) > s.Limits.MaxSegmentBytes {
		return out, fmt.Errorf("cmaf: media segment %d exceeds limit (%d > %d)", segIdx, len(buf), s.Limits.MaxSegmentBytes)
	}
	boxes, problems := WalkTopLevel(buf, s.Limits.MaxBoxBytes)
	for _, p := range problems {
		out.Problems = append(out.Problems, problemToLocated(p, segIdx))
	}

	for i := 0; i < len(boxes); i++ {
		b := boxes[i]
		switch b.Type {
		case "moof":
			out.MoofCount++
			if out.MoofCount > s.Limits.MaxFragments {
				return out, fmt.Errorf("cmaf: segment %d exceeds max fragments %d", segIdx, s.Limits.MaxFragments)
			}
			frag := s.parseMoof(buf, b, segIdx)
			// Relate the following mdat, if adjacent.
			if i+1 < len(boxes) && boxes[i+1].Type == "mdat" {
				md := boxes[i+1]
				frag.PayloadBytes = uint64(md.End - md.Offset - md.HeaderSize)
				out.MdatBytes += frag.PayloadBytes
				s.resolveAvailability(buf, &frag, md, segIdx)
			} else {
				s.resolveAvailability(buf, &frag, Box{}, segIdx)
			}
			out.Fragments = append(out.Fragments, frag)
		}
	}
	if len(out.Fragments) == 0 && len(out.Problems) == 0 {
		return out, fmt.Errorf("cmaf: media segment %d has no moof", segIdx)
	}
	return out, nil
}
