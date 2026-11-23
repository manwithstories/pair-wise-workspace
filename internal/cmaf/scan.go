package cmaf

import (
	"bytes"
	"fmt"

	"github.com/abema/go-mp4"

	"mediaflow/internal/clock"
)

// Limits bounds work per segment so corrupt lengths cannot trigger unbounded
// allocation or reads.
type Limits struct {
	MaxSegmentBytes       int
	MaxBoxBytes           uint64
	MaxSamplesPerFragment int
	MaxFragments          int
}

// DefaultLimits are the limits used by the CLI and tests.
func DefaultLimits() Limits {
	return Limits{
		MaxSegmentBytes:       4 << 20,
		MaxBoxBytes:           2 << 20,
		MaxSamplesPerFragment: 4096,
		MaxFragments:          4096,
	}
}

// TrackInfo is the per-track metadata learned from the initialization
// segment's moov.
type TrackInfo struct {
	TrackID               uint32 `json:"track_id"`
	Timescale             uint32 `json:"timescale"`
	Handler               string `json:"handler"`
	Codec                 string `json:"codec"`
	Width                 uint16 `json:"width,omitempty"`
	Height                uint16 `json:"height,omitempty"`
	DefaultSampleDuration uint32 `json:"default_sample_duration"`
	DefaultSampleSize     uint32 `json:"default_sample_size"`
	DefaultSampleFlags    uint32 `json:"default_sample_flags"`
	DurationTicks         uint64 `json:"duration_ticks"`
}

// InitSegment is the parsed initialization data.
type InitSegment struct {
	SegmentIndex int                  `json:"segment_index"`
	Tracks       []TrackInfo          `json:"tracks"`
	Problems     []clock.LocatedError `json:"problems,omitempty"`
	Size         int                  `json:"size"`
}

// Sample describes one sample located in a media segment.
type Sample struct {
	TrackID         uint32 `json:"track_id"`
	Index           int    `json:"index"`
	Segment         int    `json:"segment"`
	Offset          uint64 `json:"offset"`
	Size            uint32 `json:"size"`
	Available       uint32 `json:"available"`
	DurationTicks   uint32 `json:"duration_ticks"`
	DecodeTimeTicks uint64 `json:"decode_time_ticks"`
	DecodeTimeUs    int64  `json:"decode_time_us"`
	IsKeyframe      bool   `json:"is_keyframe"`
	NonSync         bool   `json:"non_sync"`
	Truncated       bool   `json:"truncated"`
	TimeKnown       bool   `json:"time_known"`
	Flags           uint32 `json:"flags"`
	// FlagsConflict is set when a fragment's first-sample sync flag and the
	// per-sample sync flag disagree; the scanner records both readings rather
	// than choosing one.
	FlagsConflict bool `json:"flags_conflict"`
}

// Fragment is one moof+mdat pair. Samples from every traf in the moof are
// flattened here; TrackID keeps them separable.
type Fragment struct {
	Segment        int                  `json:"segment"`
	SequenceNumber uint32               `json:"sequence_number"`
	HasMfhd        bool                 `json:"has_mfhd"`
	HasTfdt        bool                 `json:"has_tfdt"`
	TfdtTicks      map[uint32]uint64    `json:"tfdt_ticks,omitempty"`
	Samples        []Sample             `json:"samples"`
	Problems       []clock.LocatedError `json:"problems,omitempty"`
	MoofOffset     uint64               `json:"moof_offset"`
	PayloadBytes   uint64               `json:"payload_bytes"`
}

// decodeBox unmarshals a box payload using go-mp4. Bounds were already
// validated by the walker, so this only guards semantic decode failures.
func decodeBox(buf []byte, b Box, dst mp4.IBox) error {
	if b.Offset < 0 || b.HeaderSize < 8 || b.End > len(buf) || b.End < b.Offset+b.HeaderSize {
		return fmt.Errorf("cmaf: box %q bounds invalid", b.Type)
	}
	payload := buf[b.Offset+b.HeaderSize : b.End]
	if len(payload) == 0 {
		return fmt.Errorf("cmaf: box %q has empty payload", b.Type)
	}
	_, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), dst, mp4.Context{})
	return err
}

func findBox(boxes []Box, typ string) (Box, bool) {
	for _, b := range boxes {
		if b.Type == typ {
			return b, true
		}
	}
	return Box{}, false
}

func children(buf []byte, parent Box) ([]Box, []Problem) {
	var problems []Problem
	kids := ParseChildren(buf, parent, &problems)
	return kids, problems
}

// childrenAfter parses children that start after a fixed-length payload
// prefix (stsd's version/flags + entry_count, for example).
func childrenAfter(buf []byte, parent Box, prefix int) ([]Box, []Problem) {
	shifted := parent
	shifted.HeaderSize += prefix
	if shifted.Offset+shifted.HeaderSize > shifted.End {
		return nil, []Problem{{Kind: clock.ErrBoxTruncated, Offset: parent.Offset, Detail: "payload prefix exceeds box"}}
	}
	var problems []Problem
	kids := ParseChildren(buf, shifted, &problems)
	return kids, problems
}

func problemToLocated(p Problem, seg int) clock.LocatedError {
	return clock.LocatedError{Kind: p.Kind, Segment: seg, Offset: uint64(p.Offset), Detail: p.Detail}
}
