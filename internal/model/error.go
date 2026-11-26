// Package model defines the integer-tick primitives and the unified error
// model shared by every broadcast pipeline subsystem. Records always keep
// the supplier-side byte offset and the original PTS so that anomalies can
// be traced back to source material rather than to pipeline processing.
package model

import (
	"fmt"
	"sort"
)

// Pipeline stage identifiers. They are stable strings so reports stay
// readable across versions and can be diffed between repeated runs.
const (
	StageManifest  = "manifest"
	StageClock     = "clock"
	StageTimeline  = "timeline"
	StageTransport = "transport"
	StageTransform = "transform"
	StageRender    = "render"
	StageVerify    = "verify"
)

// Severity ranks how the pipeline handled an anomaly.
type Severity string

const (
	// SeverityWarning: anomaly observed, pipeline continued untouched.
	SeverityWarning Severity = "warning"
	// SeverityRecovered: anomaly repaired (wrap expansion, FEC recovery ...).
	SeverityRecovered Severity = "recovered"
	// SeverityError: data loss or skipped region, other tracks continue.
	SeverityError Severity = "error"
	// SeverityFatal: input cannot be processed at all (exit code 2).
	SeverityFatal Severity = "fatal"
)

// Stable error codes. Fatal codes make the whole run non-recoverable.
const (
	ErrCorruptManifest   = "CORRUPT_MANIFEST"
	ErrUnsupportedFormat = "UNSUPPORTED_FORMAT"
	ErrIntegerOverflow   = "INTEGER_OVERFLOW"
	ErrPTSWrap           = "PTS_WRAP"
	ErrAnchorConflict    = "ANCHOR_CONFLICT"
	ErrPacketLoss        = "PACKET_LOSS"
	ErrPacketUnrecovered = "PACKET_UNRECOVERED"
	ErrDuplicatePacket   = "DUPLICATE_PACKET"
	ErrTailTruncation    = "TAIL_TRUNCATION"
	ErrChecksumFailure   = "CHECKSUM_FAILURE"
	ErrEditOutOfBounds   = "EDIT_OUT_OF_BOUNDS"
	ErrMissingTrack      = "MISSING_TRACK"
	ErrKeyframeSeek      = "KEYFRAME_SEEK"
	ErrSyncDrift         = "SYNC_DRIFT"
)

// fatalCodes marks inputs the pipeline cannot meaningfully process.
var fatalCodes = map[string]bool{
	ErrCorruptManifest:   true,
	ErrUnsupportedFormat: true,
}

// Error is the single anomaly type flowing through all three subsystems.
type Error struct {
	Stage        string   `json:"stage"`
	StreamID     int      `json:"stream_id"`
	Code         string   `json:"code"`
	Message      string   `json:"message"`
	Severity     Severity `json:"severity"`
	SourceOffset int64    `json:"source_offset"`
	OriginalPTS  int64    `json:"original_pts"`
	HasPTS       bool     `json:"has_pts"`
	WrapCount    int      `json:"wrap_count,omitempty"`
	Recovered    bool     `json:"recovered"`
}

// Error implements the error interface with full provenance.
func (e Error) Error() string {
	base := fmt.Sprintf("[%s/%s] stream=%d %s", e.Stage, e.Code, e.StreamID, e.Message)
	if e.SourceOffset >= 0 {
		base += fmt.Sprintf(" source_offset=%d", e.SourceOffset)
	}
	if e.HasPTS {
		base += fmt.Sprintf(" original_pts=%d", e.OriginalPTS)
	}
	return base
}

// Fatal reports whether this record aborts the whole run.
func (e Error) Fatal() bool {
	return e.Severity == SeverityFatal || fatalCodes[e.Code]
}

// NewError builds a record with provenance. Pass pts with hasPTS=false when
// the anomaly happens before any PTS is known.
func NewError(stage string, streamID int, code string, severity Severity, msg string, sourceOffset, pts int64, hasPTS bool) Error {
	return Error{
		Stage:        stage,
		StreamID:     streamID,
		Code:         code,
		Message:      msg,
		Severity:     severity,
		SourceOffset: sourceOffset,
		OriginalPTS:  pts,
		HasPTS:       hasPTS,
	}
}

// SortErrors orders records deterministically so identical seeds always
// produce byte-identical reports regardless of goroutine scheduling.
func SortErrors(errs []Error) {
	sort.SliceStable(errs, func(i, j int) bool {
		a, b := errs[i], errs[j]
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		if a.StreamID != b.StreamID {
			return a.StreamID < b.StreamID
		}
		if a.SourceOffset != b.SourceOffset {
			return a.SourceOffset < b.SourceOffset
		}
		if a.HasPTS != b.HasPTS {
			return !a.HasPTS // records without PTS first
		}
		if a.OriginalPTS != b.OriginalPTS {
			return a.OriginalPTS < b.OriginalPTS
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
}

// HasFatal reports whether the collected records contain a fatal input.
func HasFatal(errs []Error) bool {
	for _, e := range errs {
		if e.Fatal() {
			return true
		}
	}
	return false
}

// CountByCode returns how many records carry each code.
func CountByCode(errs []Error) map[string]int {
	out := make(map[string]int)
	for _, e := range errs {
		out[e.Code]++
	}
	return out
}
