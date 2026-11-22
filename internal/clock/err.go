package clock

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable error code shared by all modules.
type Code string

const (
	CodeManifestInvalid   Code = "MANIFEST_INVALID"
	CodeNegativeDuration  Code = "DURATION_NEGATIVE"
	CodeTimescaleInvalid  Code = "TIMESCALE_INVALID"
	CodeTickOverflow      Code = "TICK_OVERFLOW"
	CodeCodecUnsupported  Code = "CODEC_UNSUPPORTED"
	CodePathEscape        Code = "PATH_ESCAPE"
	CodeManifestTooLarge  Code = "MANIFEST_TOO_LARGE"
	CodeTruncatedSegment  Code = "TRUNCATED_SEGMENT"
	CodeCheckpointCorrupt Code = "CHECKPOINT_CORRUPT"
	CodeDependencyMissing Code = "DEPENDENCY_MISSING"
	CodeInputQuality      Code = "INPUT_QUALITY"
	CodeIO                Code = "IO_ERROR"
	CodeUnrecoverable     Code = "UNRECOVERABLE"
)

// Error is the shared error model: a stable code plus the track and
// segment position that produced the failure.
type Error struct {
	Code    Code
	Op      string // operation, e.g. "parse-hls", "cut"
	Track   string // track ID, empty if not applicable
	Segment int    // segment index, -1 if not applicable
	Msg     string
	Err     error // wrapped cause
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: op=%s", e.Code, e.Op)
	if e.Track != "" {
		s += " track=" + e.Track
	}
	if e.Segment >= 0 {
		s += fmt.Sprintf(" segment=%d", e.Segment)
	}
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// At builds an Error located at a track/segment position.
func At(code Code, op, track string, segment int, format string, args ...any) *Error {
	return &Error{Code: code, Op: op, Track: track, Segment: segment, Msg: fmt.Sprintf(format, args...)}
}

// Wrap attaches a code and operation to a lower-level error.
func Wrap(code Code, op string, err error) *Error {
	return &Error{Code: code, Op: op, Segment: -1, Err: err}
}

// CodeOf extracts the first Code found in the error chain, or "".
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
