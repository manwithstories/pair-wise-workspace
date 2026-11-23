// Package diag defines the shared, machine-parseable error model used by every
// pipeline stage: parsing, scheduling, alignment, output and checkpointing.
//
// A diagnostic always carries a stable ErrorCode (so operators can branch on it
// programmatically), the affected track, a byte offset inside the input stream
// when applicable, and a human readable message. Diagnostics serialize as a
// single JSON object per line (JSON Lines) to stdout/stderr.
package diag

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrorCode is the stable, machine readable identifier of a failure class.
type ErrorCode string

const (
	// EParam covers rejected parameters and values beyond configured limits.
	EParam ErrorCode = "E_PARAM"
	// ESubtitleTruncated marks a cue whose block ends before its payload.
	ESubtitleTruncated ErrorCode = "E_SUBTITLE_TRUNCATED"
	// ESubtitleSyntax marks malformed timing/cue lines that are fully present.
	ESubtitleSyntax ErrorCode = "E_SUBTITLE_SYNTAX"
	// ETimecode marks illegal SMPTE timecode components or frame numbers.
	ETimecode ErrorCode = "E_TIMECODE"
	// ERTP marks malformed RTP packets or out-of-limit timestamp jumps.
	ERTP ErrorCode = "E_RTP"
	// EInputCorrupt marks a damaged input byte stream with a located offset.
	EInputCorrupt ErrorCode = "E_INPUT_CORRUPT"
	// EGraphCycle marks a dependency cycle detected before scheduling starts.
	EGraphCycle ErrorCode = "E_GRAPH_CYCLE"
	// EGraphMissing marks a dependency on an unknown track.
	EGraphMissing ErrorCode = "E_GRAPH_MISSING"
	// ENoKeyframe marks an alignment window without any usable keyframe.
	ENoKeyframe ErrorCode = "E_NO_KEYFRAME"
	// EOutput marks a local disk write/rename failure.
	EOutput ErrorCode = "E_OUTPUT"
	// EChecksum marks a checksum mismatch against committed output.
	EChecksum ErrorCode = "E_CHECKSUM"
	// EResume marks a checkpoint that cannot be replayed consistently.
	EResume ErrorCode = "E_RESUME"
)

// Error is the structured diagnostic shared by all pipeline modules.
type Error struct {
	Code   ErrorCode `json:"code"`
	Track  string    `json:"track,omitempty"`
	Offset int64     `json:"offset,omitempty"`
	Msg    string    `json:"msg"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Track != "" && e.Offset >= 0 {
		return fmt.Sprintf("%s: track=%s offset=%d: %s", e.Code, e.Track, e.Offset, e.Msg)
	}
	if e.Track != "" {
		return fmt.Sprintf("%s: track=%s: %s", e.Code, e.Track, e.Msg)
	}
	if e.Offset >= 0 {
		return fmt.Sprintf("%s: offset=%d: %s", e.Code, e.Offset, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

// JSON emits the diagnostic as one compact JSON object (no trailing newline).
func (e *Error) JSON() string {
	b, err := json.Marshal(e)
	if err != nil {
		return `{"code":"E_PARAM","msg":"diagnostic encode failed"}`
	}
	return string(b)
}

// New builds a diagnostic without a stream offset.
func New(code ErrorCode, track, format string, args ...any) *Error {
	return &Error{Code: code, Track: track, Offset: -1, Msg: fmt.Sprintf(format, args...)}
}

// At builds a diagnostic anchored at a byte offset inside a track's stream.
func At(code ErrorCode, track string, offset int64, format string, args ...any) *Error {
	return &Error{Code: code, Track: track, Offset: offset, Msg: fmt.Sprintf(format, args...)}
}

// CodeOf extracts the ErrorCode from any error, returning the empty string for
// a plain (non-diagnostic) error and treating nil as no error.
func CodeOf(err error) ErrorCode {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// OffsetOf extracts the located byte offset, or -1 when the error carries none.
func OffsetOf(err error) int64 {
	var de *Error
	if errors.As(err, &de) {
		return de.Offset
	}
	return -1
}

// ExitCode maps a terminal pipeline error onto the documented process contract:
//
//	0: success, 2: located input corruption, 3: resume/checksum mismatch,
//	4: parameter/limit rejection, 1: every other failure.
func ExitCode(err error) int {
	switch CodeOf(err) {
	case "":
		return 0
	case EInputCorrupt, ERTP, ESubtitleTruncated, ESubtitleSyntax, ETimecode:
		return 2
	case EChecksum, EResume:
		return 3
	case EParam:
		return 4
	default:
		return 1
	}
}

// Limits centralizes every hard cap so the pipeline never relies on unbounded
// buffers. Zero values in a caller are replaced by the defaults below.
type Limits struct {
	// MaxTimestampMS rejects media timestamps beyond this point in time.
	MaxTimestampMS int64
	// MaxCueLines rejects subtitle inputs carrying more than this many lines.
	MaxCueLines int
	// MaxQueueCapacity rejects requested bounded-queue sizes above this value.
	MaxQueueCapacity int
	// MaxTracks rejects track graphs with more than this many vertices.
	MaxTracks int
}

// DefaultLimits is the conservative, demo-sized cap set.
func DefaultLimits() Limits {
	return Limits{
		MaxTimestampMS:   6 * 60 * 60 * 1000, // 6 hours
		MaxCueLines:      200_000,
		MaxQueueCapacity: 4096,
		MaxTracks:        64,
	}
}

// CheckQueue rejects a queue capacity outside [1, MaxQueueCapacity].
func (l Limits) CheckQueue(capacity int) error {
	if capacity < 1 {
		return New(EParam, "", "queue capacity must be >= 1, got %d", capacity)
	}
	if capacity > l.MaxQueueCapacity {
		return New(EParam, "", "queue capacity %d exceeds limit %d", capacity, l.MaxQueueCapacity)
	}
	return nil
}

// CheckTimestampMS rejects timestamps past the configured horizon.
func (l Limits) CheckTimestampMS(track string, ms int64) error {
	if ms < 0 {
		return New(EParam, track, "negative timestamp %dms", ms)
	}
	if ms > l.MaxTimestampMS {
		return New(EParam, track, "timestamp %dms exceeds limit %dms", ms, l.MaxTimestampMS)
	}
	return nil
}

// CheckCueLines rejects subtitle inputs that exceed the line budget.
func (l Limits) CheckCueLines(track string, lines int) error {
	if lines > l.MaxCueLines {
		return At(EParam, track, -1, "subtitle line count %d exceeds limit %d", lines, l.MaxCueLines)
	}
	return nil
}
