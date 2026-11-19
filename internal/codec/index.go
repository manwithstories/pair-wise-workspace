// Package codec validates raw elementary-stream keyframe indexes for H.264,
// H.265 and AV1. An index entry points at a byte offset inside the stream and
// declares the presentation timestamp of a sync sample. Every rejection is
// explicit: out-of-range offsets, negative timestamps and non-monotonic
// indexes never pass silently.
package codec

import (
	"fmt"
	"sort"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// Format identifies a raw bitstream codec.
type Format string

const (
	H264 Format = "h264"
	H265 Format = "h265"
	AV1  Format = "av1"
)

// ParseFormat converts CLI text into a format, rejecting unknown values.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case H264, H265, AV1:
		return Format(s), nil
	default:
		return "", &core.Error{Code: core.CodeBadArgument, Detail: "unsupported codec format: " + s, Fatal: true}
	}
}

// IsKeyframe reports whether the codec's bitstream headers mark a sync sample.
func (f Format) IsKeyframe(data []byte) bool {
	switch f {
	case H264:
		return h264HasIDR(data)
	case H265:
		return h265HasIRAP(data)
	case AV1:
		return av1IsKeyframe(data)
	default:
		return false
	}
}

// Sample is one keyframe candidate presented on the shared timeline.
type Sample struct {
	Offset int64           `json:"offset"`
	Size   int             `json:"size"`
	PTS    timeline.Micros `json:"pts_us"`
	Key    bool            `json:"keyframe"`
}

// Index is the validated keyframe index for one track.
type Index struct {
	Track  string            `json:"track"`
	Format Format            `json:"format"`
	Length int               `json:"length"`
	Base   timeline.TimeBase `json:"-"`
	Origin timeline.Micros   `json:"origin_us"`
	Sync   []Sample          `json:"sync"`
	// Rejects lists non-fatal anomalies (for example a keyframe that fails
	// format validation) that did not abort the whole index.
	Rejects []*core.Error `json:"rejects,omitempty"`
}

// MaxSamples guards against index bombs.
const MaxSamples = 100_000

// Verify validates an index against the raw bitstream it describes. It checks
// the size, every byte range, every declared timestamp and strictly
// increasing PTS values. Any violation is returned as a fatal validation error
// with the offending track attached.
func Verify(track string, format Format, stream []byte, base timeline.TimeBase, samples []Sample) (*Index, error) {
	if track == "" {
		return nil, &core.Error{Code: core.CodeBadArgument, Detail: "empty track id", Fatal: true}
	}
	if !base.Valid() {
		return nil, &core.Error{Code: core.CodeBadTimestamp, Track: track, Detail: "unset time base", Fatal: true}
	}
	if len(samples) == 0 {
		return nil, &core.Error{Code: core.CodeEmptyTrack, Track: track, Detail: "keyframe index is empty", Fatal: true}
	}
	if len(samples) > MaxSamples {
		return nil, &core.Error{Code: core.CodeOverLimit, Track: track, Detail: "keyframe index too large", Fatal: true}
	}
	idx := &Index{Track: track, Format: format, Length: len(stream), Base: base, Origin: base.Origin}
	prevPTS := timeline.Micros(-1)
	for i, s := range samples {
		if s.PTS < 0 {
			return nil, &core.Error{Code: core.CodeBadTimestamp, Track: track,
				Detail: fmt.Sprintf("keyframe %d has negative pts", i), Fatal: true}
		}
		if s.Offset < 0 {
			return nil, &core.Error{Code: core.CodeBadIndex, Track: track,
				Detail: fmt.Sprintf("keyframe %d has negative byte offset", i), Fatal: true}
		}
		if s.Size <= 0 {
			return nil, &core.Error{Code: core.CodeBadIndex, Track: track,
				Detail: fmt.Sprintf("keyframe %d has non-positive size", i), Fatal: true}
		}
		off := int(s.Offset)
		if off > len(stream) || s.Size > len(stream)-off {
			return nil, &core.Error{Code: core.CodeBadIndex, Track: track,
				Detail: fmt.Sprintf("keyframe %d range [%d,%d) exceeds %d-byte stream",
					i, off, off+s.Size, len(stream)), Fatal: true}
		}
		if s.PTS <= prevPTS {
			return nil, &core.Error{Code: core.CodeBadIndex, Track: track,
				Detail: fmt.Sprintf("keyframe %d pts %d is not increasing after %d",
					i, s.PTS, prevPTS), Fatal: true}
		}
		if i > 0 && s.Offset < samples[i-1].Offset {
			return nil, &core.Error{Code: core.CodeBadIndex, Track: track,
				Detail: fmt.Sprintf("keyframe %d byte offset is not monotonic", i), Fatal: true}
		}
		prevPTS = s.PTS
		key := format.IsKeyframe(stream[off : off+s.Size])
		if s.Key && !key {
			// Declared keyframe that the bitstream does not confirm.
			idx.Rejects = append(idx.Rejects, &core.Error{Code: core.CodeBadIndex, Track: track,
				Detail: fmt.Sprintf("declared keyframe %d lacks a sync header", i), Fatal: false})
			key = false
		}
		idx.Sync = append(idx.Sync, Sample{Offset: s.Offset, Size: s.Size, PTS: s.PTS, Key: key})
	}
	if len(idx.Sync) == 0 {
		return nil, &core.Error{Code: core.CodeBadIndex, Track: track, Detail: "index has no usable sync samples", Fatal: true}
	}
	sort.Slice(idx.Sync, func(i, j int) bool { return idx.Sync[i].PTS < idx.Sync[j].PTS })
	return idx, nil
}

// Seek returns the index entry covering a presentation time, or the last
// keyframe at or before it. ok is false when t precedes the first keyframe.
func (idx *Index) Seek(t timeline.Micros) (*Sample, bool) {
	if idx == nil || len(idx.Sync) == 0 {
		return nil, false
	}
	lo, hi := 0, len(idx.Sync)
	for lo < hi {
		mid := (lo + hi) / 2
		if idx.Sync[mid].PTS <= t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return nil, false
	}
	s := idx.Sync[lo-1]
	return &s, true
}

// SampleAt returns the exact keyframe whose PTS equals t.
func (idx *Index) SampleAt(t timeline.Micros) (*Sample, bool) {
	if idx == nil {
		return nil, false
	}
	for i := range idx.Sync {
		if idx.Sync[i].PTS == t {
			return &idx.Sync[i], true
		}
	}
	return nil, false
}

// Count returns the number of usable sync samples.
func (idx *Index) Count() int {
	if idx == nil {
		return 0
	}
	return len(idx.Sync)
}
