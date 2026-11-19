// Package fault defines the single error model shared by every avpipe stage.
//
// Every failure carries a stable code, an operation, an optional track name and
// byte offset. Wrapped causes form a chain; Root always returns the first root
// cause so dependency propagation cannot hide the original failure.
package fault

import (
	"errors"
	"fmt"
	"strings"
)

// Code identifies a failure class. Codes are stable across releases.
type Code string

const (
	CodeInvalidArgument   Code = "invalid_argument"
	CodeInvalidInput      Code = "invalid_input"
	CodeWAVTruncated      Code = "wav_truncated"
	CodeWAVFormat         Code = "wav_format"
	CodeWAVLength         Code = "wav_length"
	CodeUnsupportedLayout Code = "unsupported_layout"
	CodeResample          Code = "resample"
	CodeGraphCycle        Code = "graph_cycle"
	CodeGraphMissing      Code = "graph_missing_dependency"
	CodeGraphDuplicate    Code = "graph_duplicate_node"
	CodeGraphMonotonic    Code = "graph_non_monotonic"
	CodeQueueClosed       Code = "queue_closed"
	CodeQueueCanceled     Code = "queue_canceled"
	CodeQueueLimit        Code = "queue_limit"
	CodeRTP               Code = "rtp_packet"
	CodeRecoveryWindow    Code = "recovery_window"
	CodeSync              Code = "sync_align"
	CodeTrackFailed       Code = "track_failed"
	CodeMetric            Code = "metric_threshold"
	CodeInternal          Code = "internal"
)

// Class groups codes by the process exit status they imply.
type Class int

const (
	ClassInput Class = iota
	ClassMetric
	ClassInternal
)

// Class reports how a code should be mapped to an exit status.
func (c Code) Class() Class {
	switch c {
	case CodeMetric:
		return ClassMetric
	case CodeInternal, CodeRTP:
		return ClassInternal
	default:
		return ClassInput
	}
}

// ExitCode maps an error to the documented CLI status: 0 ok, 2 input,
// 3 metric, 4 internal.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	e := Root(err)
	if e == nil {
		return 4
	}
	switch e.Code.Class() {
	case ClassInput:
		return 2
	case ClassMetric:
		return 3
	default:
		return 4
	}
}

// Error is the unified, locatable error value used across avpipe.
type Error struct {
	Code      Code
	Op        string
	Track     string
	Offset    int64
	HasOffset bool
	Detail    string
	Cause     error
}

// New builds a leaf error.
func New(code Code, op, format string, args ...any) *Error {
	return &Error{Code: code, Op: op, Detail: fmt.Sprintf(format, args...)}
}

// Wrap attaches a cause and formats a detail line.
func Wrap(code Code, op string, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Op: op, Detail: fmt.Sprintf(format, args...), Cause: cause}
}

// WithTrack annotates the error with a logical track name.
func (e *Error) WithTrack(track string) *Error {
	e.Track = track
	return e
}

// At annotates the error with a byte offset in the input stream.
func (e *Error) At(offset int64) *Error {
	e.Offset, e.HasOffset = offset, true
	return e
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(string(e.Code))
	if e.Track != "" {
		b.WriteString(" track=")
		b.WriteString(e.Track)
	}
	if e.HasOffset {
		fmt.Fprintf(&b, " offset=%d", e.Offset)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap exposes the cause chain.
func (e *Error) Unwrap() error { return e.Cause }

// Root returns the deepest *Error in a chain (the first root cause).
func Root(err error) *Error {
	if err == nil {
		return nil
	}
	cur := err
	for {
		var e *Error
		if !errors.As(cur, &e) {
			return &Error{Code: CodeInternal, Op: "unclassified", Detail: cur.Error(), Cause: cur}
		}
		if e.Cause == nil {
			return e
		}
		cur = e.Cause
	}
}

// FirstRoot preserves the first non-nil root cause among candidates.
func FirstRoot(causes ...error) *Error {
	for _, cause := range causes {
		if cause == nil || errors.Is(cause, nil) {
			continue
		}
		return Root(cause)
	}
	return nil
}
