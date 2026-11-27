// Package media holds the shared time base and error model used by both the
// fragment parser/recovery stage and the timeline fan-in stage.
package media

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// TrackKind identifies the elementary stream carried by a track.
type TrackKind string

const (
	KindVideo TrackKind = "video"
	KindAudio TrackKind = "audio"
)

// TimeScale is a media timescale expressed in ticks per second.
type TimeScale uint32

// Valid reports whether the timescale is usable (non-zero and non-absurd).
func (ts TimeScale) Valid() bool { return ts > 0 && ts <= 100_000_000 }

// ToMicros converts a tick count on this timescale into microseconds using
// 64-bit arithmetic. It rounds to the nearest microsecond and saturates
// instead of overflowing.
func (ts TimeScale) ToMicros(ticks int64) int64 {
	if ts == 0 {
		return 0
	}
	perSec := int64(ts)
	if ticks > math.MaxInt64/perSec && ticks > 0 {
		return math.MaxInt64 / 2
	}
	if ticks < math.MinInt64/perSec && ticks < 0 {
		return math.MinInt64 / 2
	}
	return roundDiv(ticks*1_000_000, perSec)
}

// FromMicros converts microseconds to ticks on this timescale.
func (ts TimeScale) FromMicros(us int64) int64 {
	if ts == 0 {
		return 0
	}
	if us > math.MaxInt64/1_000_000 || us < math.MinInt64/1_000_000 {
		// Defensive: callers clamp before asking for absurd conversions.
		if us > 0 {
			return math.MaxInt64 / int64(ts)
		}
		return math.MinInt64 / int64(ts)
	}
	return roundDiv(us*int64(ts), 1_000_000)
}

func roundDiv(num, den int64) int64 {
	if den < 0 {
		num, den = -num, -den
	}
	if num >= 0 {
		return (num + den/2) / den
	}
	return -((-num + den/2) / den)
}

// Sample is one coded frame or audio frame as indexed from the bitstream.
type Sample struct {
	// LocalIndex is the fragment-local sample ordinal as found in trun.
	LocalIndex int
	// Segment is the original mfhd sequence number of the fragment.
	Segment uint32
	// TrackID is the container track id.
	TrackID uint32
	// Timescale is the media timescale of the track.
	Timescale TimeScale
	// DTS/CTS are the raw decode/composition ticks as stored in tfdt/trun.
	DTS int64
	CTS int64
	// Duration is the sample duration in ticks.
	Duration int64
	// Size is the sample payload size in bytes.
	Size int
	// DataOffset is the absolute file offset of the sample payload.
	DataOffset int64
	// Keyframe reports whether the sample was flagged as sync.
	Keyframe bool
}

// StartMicros is the composition start time in shared-clock microseconds.
func (s Sample) StartMicros() int64 { return s.Timescale.ToMicros(s.CTS) }

// EndMicros is the composition end time in shared-clock microseconds.
func (s Sample) EndMicros() int64 { return s.Timescale.ToMicros(s.CTS + s.Duration) }

// Track is the parsed description of one track inside a representation.
type Track struct {
	ID        uint32
	Kind      TrackKind
	Timescale TimeScale
	// HandlerType is the hdlr type such as "vide" or "soun".
	HandlerType string
	// Codec is "avc1", "avc3", "mp4a" ...
	Codec string
	// InitSize is the length of the decoder configuration record in the stsd.
	InitSize int
	Width    int
	Height   int
}

// Representation is the parsed shape of one CMAF/fMP4 representation file.
type Representation struct {
	Name     string
	Path     string
	FileSize int64
	// MovieTimescale is the mvhd timescale.
	MovieTimescale TimeScale
	Segments       []Segment
	Tracks         []Track
}

// Segment groups one moof plus its following mdat.
type Segment struct {
	Sequence      uint32
	MoofOffset    int64
	MoofSize      int64
	MdatOffset    int64
	MdatSize      int64
	MdatTruncated bool
	FirstBadByte  int64 // -1 when the segment parsed cleanly
	SampleCount   int
}

// ErrFirstBadByte is returned by the parser when a structural defect is found.
// Offset is the first byte that could not be validated.
type ErrFirstBadByte struct {
	Offset int64
	Reason string
}

func (e *ErrFirstBadByte) Error() string {
	return fmt.Sprintf("first_bad_byte=%d reason=%s", e.Offset, e.Reason)
}

// FirstBadByteOffset extracts the offending offset from any error chain.
func FirstBadByteOffset(err error) (int64, bool) {
	var bad *ErrFirstBadByte
	if errors.As(err, &bad) {
		return bad.Offset, true
	}
	return -1, false
}

// ErrUnrecoverable marks input that cannot be turned into a usable timeline.
var ErrUnrecoverable = errors.New("unrecoverable input")

// Unrecoverable wraps a cause with ErrUnrecoverable, preserving the chain so
// first_bad_byte survives errors.Is checks.
func Unrecoverable(cause error) error {
	if cause == nil {
		return ErrUnrecoverable
	}
	return fmt.Errorf("%w: %v", ErrUnrecoverable, cause)
}

// DriftEstimate is a linear clock fit recovered from sample timestamps.
type DriftEstimate struct {
	// OriginTick is the first DTS observed before wrap expansion.
	OriginTick int64
	// OriginMicros is the shared-clock anchor for OriginTick.
	OriginMicros int64
	// SlopePPM is the least-squares slope in parts per million.
	SlopePPM float64
	// ResidualMicros is the largest absolute residual after correction.
	ResidualMicros int64
	// Samples is the number of points used for the fit.
	Samples int
}

// Apply maps a raw tick to corrected microseconds using the fitted clock.
func (d DriftEstimate) Apply(ts TimeScale, ticks int64) int64 {
	raw := ts.ToMicros(ticks)
	delta := raw - d.OriginMicros
	corrected := d.OriginMicros + delta
	if d.SlopePPM != 0 {
		corrected = d.OriginMicros + delta - int64(float64(delta)*d.SlopePPM/1e6)
	}
	_ = d.OriginTick
	return corrected
}

// Timeout budgets. All operations are bounded so a stuck child cannot hang a run.
const (
	ParseBudget    = 200 * time.Millisecond
	DecodeBudget   = 60 * time.Second / 2
	DigestChunkMeg = 1 << 20
)

// MaxSampleBytes bounds a single sample to keep peak memory under 256 MiB.
const MaxSampleBytes = 64 << 20

// TimelineSample is one sample after fan-in: it carries both the recovered
// per-representation timing and the shared-clock correction applied.
type TimelineSample struct {
	Sample
	Representation string        `json:"representation"`
	StartMicros    int64         `json:"start_us"`
	EndMicros      int64         `json:"end_us"`
	Drift          DriftEstimate `json:"drift"`
}

// TrackTimeline is a single track's samples on the shared clock, ordered by
// composition time then decode time.
type TrackTimeline struct {
	TrackID   uint32           `json:"track_id"`
	Kind      TrackKind        `json:"kind"`
	Timescale TimeScale        `json:"timescale"`
	Samples   []TimelineSample `json:"samples"`
	StartUS   int64            `json:"start_us"`
	EndUS     int64            `json:"end_us"`
}

// Timeline is the fanned-in, shared-clock result handed to decode and verify.
type Timeline struct {
	// OriginUS is the common zero of the shared clock.
	OriginUS int64           `json:"origin_us"`
	Tracks   []TrackTimeline `json:"tracks"`
	// Boundaries records each representation's declared track boundaries.
	Boundaries []Boundary `json:"boundaries"`
	// Conflicts is non-empty when representations disagree past the threshold.
	Conflicts []Conflict `json:"conflicts"`
	// Representations lists the source representations in fan-in order.
	Representations []string `json:"representations"`
	// Alignments is the per-representation fragment boundary comparison.
	Alignments []Alignment `json:"alignments,omitempty"`
}

// Alignment summarises fragment-boundary alignment between two representations.
type Alignment struct {
	Kind          TrackKind `json:"kind"`
	RepA          string    `json:"rep_a"`
	RepB          string    `json:"rep_b"`
	Matched       int       `json:"matched"`
	UnmatchedA    int       `json:"unmatched_a"`
	UnmatchedB    int       `json:"unmatched_b"`
	MaxDeltaUS    int64     `json:"max_delta_us"`
	OuterDeltaUS  int64     `json:"outer_delta_us"`
	MedianDeltaUS int64     `json:"median_delta_us"`
}

// Boundary is one track's first and last sample in shared time.
type Boundary struct {
	Representation string    `json:"representation"`
	TrackID        uint32    `json:"track_id"`
	Kind           TrackKind `json:"kind"`
	Sequence       uint32    `json:"sequence"`
	StartUS        int64     `json:"start_us"`
	EndUS          int64     `json:"end_us"`
	SampleCount    int       `json:"sample_count"`
}

// Conflict is a boundary mismatch beyond the configured threshold.
type Conflict struct {
	Kind      TrackKind `json:"kind"`
	Track     uint32    `json:"track_id"`
	Metric    string    `json:"metric"`
	DeltaUS   int64     `json:"delta_us"`
	Threshold int64     `json:"threshold_us"`
	Detail    string    `json:"detail"`
}
