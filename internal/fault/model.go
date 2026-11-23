// Package fault defines the canonical error model shared by the MP4, RTP and
// metadata subsystems. Every fault carries the affected track, a position on
// that track and a timestamp on the unified clock grid so findings from
// different subsystems can be merged onto one timeline.
package fault

import (
	"errors"
	"fmt"
)

// Kind enumerates the supported fault categories.
type Kind string

const (
	// KindTruncatedBox: an MP4 box header/body runs past the file boundary.
	KindTruncatedBox Kind = "truncated_box"
	// KindBadBoxDepth: box nesting exceeds the configured depth limit.
	KindBadBoxDepth Kind = "box_depth_exceeded"
	// KindBadSample: sample duration is negative, zero or inconsistent.
	KindBadSample Kind = "bad_sample"
	// KindKeyframeGap: distance between two keyframes exceeds the limit.
	KindKeyframeGap Kind = "keyframe_gap"
	// KindRTPSeqGap: RTP sequence discontinuity (missing packets).
	KindRTPSeqGap Kind = "rtp_seq_gap"
	// KindRTPWrap: 32-bit RTP timestamp wrap was resolved.
	KindRTPWrap Kind = "rtp_timestamp_wrap"
	// KindAmbiguous: RTP timestamp jump that cannot be uniquely resolved.
	KindAmbiguous Kind = "ambiguous_span"
	// KindClockDrift: track drift against the reference timeline over threshold.
	KindClockDrift Kind = "clock_drift"
	// KindInvalidTimescale: timescale/clock rate outside the valid range.
	KindInvalidTimescale Kind = "invalid_timescale"
	// KindLimitExceeded: a configured structural limit was violated.
	KindLimitExceeded Kind = "limit_exceeded"
)

// Severity ranks faults for stable sorting and reporting.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarning
	SeverityError
	SeverityFatal
)

// String renders severity for terminal output.
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarning:
		return "warning"
	case SeverityError:
		return "error"
	case SeverityFatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// Fault is one localized finding. All positions are track-local and every
// timestamp uses the unified microsecond grid (internal/clock).
type Fault struct {
	Kind      Kind     `json:"kind"`
	Severity  Severity `json:"severity"`
	TrackID   uint32   `json:"track_id"`
	TrackName string   `json:"track_name,omitempty"`
	// FrameIndex is the zero-based sample/packet index on the track, or -1
	// when the fault is not attributable to a single frame.
	FrameIndex int64 `json:"frame_index"`
	// OffsetBytes is the byte offset inside the source container/packet
	// stream, or -1 when not applicable (pure RTP timing faults).
	OffsetBytes int64 `json:"offset_bytes"`
	// Ticks is the position on the unified microsecond timeline.
	Ticks int64 `json:"ticks"`
	// SpanFrames / SpanBytes describe the damaged extent starting at
	// FrameIndex/OffsetBytes. Zero means a point fault.
	SpanFrames int64 `json:"span_frames,omitempty"`
	SpanBytes  int64 `json:"span_bytes,omitempty"`
	Message    string `json:"message"`
}

// New builds a Fault with the common position defaults applied.
func New(kind Kind, sev Severity, trackID uint32, frame, offset, ticks int64, msg string) Fault {
	return Fault{
		Kind:        kind,
		Severity:    sev,
		TrackID:     trackID,
		FrameIndex:  frame,
		OffsetBytes: offset,
		Ticks:       ticks,
		Message:     msg,
	}
}

// WithSpan attaches a damaged extent and returns the updated fault.
func (f Fault) WithSpan(frames, bytes int64) Fault {
	f.SpanFrames = frames
	f.SpanBytes = bytes
	return f
}

// WithName attaches a human-readable track name.
func (f Fault) WithName(name string) Fault {
	f.TrackName = name
	return f
}

// Error makes Fault satisfy the error interface for subsystem boundaries.
func (f Fault) Error() string {
	return fmt.Sprintf("fault kind=%s track=%d frame=%d offset=%d ticks=%d: %s",
		f.Kind, f.TrackID, f.FrameIndex, f.OffsetBytes, f.Ticks, f.Message)
}

// ErrNoFaults is returned by helpers that expect at least one finding.
var ErrNoFaults = errors.New("fault: no faults recorded")

// FirstByKind returns the first fault of the given kind, or false.
func FirstByKind(faults []Fault, kind Kind) (Fault, bool) {
	for _, f := range faults {
		if f.Kind == kind {
			return f, true
		}
	}
	return Fault{}, false
}

// ByTrack partitions faults by track ID while preserving relative order.
func ByTrack(faults []Fault) map[uint32][]Fault {
	out := make(map[uint32][]Fault)
	for _, f := range faults {
		out[f.TrackID] = append(out[f.TrackID], f)
	}
	return out
}

// OrderKey is the deterministic total order used everywhere findings are
// merged: time first, then track, then byte offset, then kind. It returns
// -1/0/1 so callers can compose it with their own tie-breakers.
func OrderKey(a, b Fault) int {
	if a.Ticks != b.Ticks {
		if a.Ticks < b.Ticks {
			return -1
		}
		return 1
	}
	if a.TrackID != b.TrackID {
		if a.TrackID < b.TrackID {
			return -1
		}
		return 1
	}
	if a.OffsetBytes != b.OffsetBytes {
		if a.OffsetBytes < b.OffsetBytes {
			return -1
		}
		return 1
	}
	if a.FrameIndex != b.FrameIndex {
		if a.FrameIndex < b.FrameIndex {
			return -1
		}
		return 1
	}
	if a.Kind != b.Kind {
		if a.Kind < b.Kind {
			return -1
		}
		return 1
	}
	return 0
}
