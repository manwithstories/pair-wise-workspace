// Package fault defines the unified error type used across all subsystems.
// Every material or quality failure carries a stable code, the track it
// belongs to, the 90 kHz tick at which it was detected and whether the
// pipeline may recover (resume) from it.
package fault

import "fmt"

// Code is a stable machine-readable error identifier.
type Code string

const (
	CodeTruncated       Code = "TRUNCATED_MEDIA"
	CodeMalformedEvent  Code = "MALFORMED_EVENT"
	CodeMissingTrack    Code = "MISSING_TRACK"
	CodeIndexCorrupt    Code = "INDEX_CORRUPT"
	CodeIndexStale      Code = "INDEX_STALE"
	CodeNegativeDur     Code = "NEGATIVE_DURATION"
	CodeOverflow        Code = "TICK_OVERFLOW"
	CodeBackwardPTS     Code = "BACKWARD_TIMESTAMP"
	CodeChannelLayout   Code = "UNKNOWN_CHANNEL_LAYOUT"
	CodeColorConflict   Code = "COLOR_METADATA_CONFLICT"
	CodeDriftExceeded   Code = "BITRATE_DRIFT_EXCEEDED"
	CodeAlignExceeded   Code = "ALIGNMENT_EXCEEDED"
	CodeEventMissing    Code = "EVENT_MISSING"
	CodeSegmentCorrupt  Code = "SEGMENT_CORRUPT"
	CodeRuntime         Code = "RUNTIME"
)

// Error is the single error carrier for material and quality failures.
type Error struct {
	Code        Code   `json:"code"`
	Track       string `json:"track,omitempty"`
	Tick        int64  `json:"tick"` // 90 kHz tick where detected; -1 when not applicable
	Recoverable bool   `json:"recoverable"`
	Msg         string `json:"msg"`
}

func (e *Error) Error() string {
	if e.Track != "" {
		return fmt.Sprintf("%s track=%s tick=%d: %s", e.Code, e.Track, e.Tick, e.Msg)
	}
	return fmt.Sprintf("%s tick=%d: %s", e.Code, e.Tick, e.Msg)
}

// New builds a fault with code, track and tick context.
func New(code Code, track string, tick int64, recoverable bool, format string, args ...any) *Error {
	return &Error{Code: code, Track: track, Tick: tick, Recoverable: recoverable, Msg: fmt.Sprintf(format, args...)}
}

// Of extracts a *fault.Error from err, returning nil when err is a plain error.
func Of(err error) *Error {
	if err == nil {
		return nil
	}
	if fe, ok := err.(*Error); ok {
		return fe
	}
	return nil
}

// IsFault reports whether err is a material/quality fault (exit 1)
// rather than a runtime failure (exit 2).
func IsFault(err error) bool { return Of(err) != nil }
