// Package clock shared error model: a stable code, the first offending byte
// (or -1) and an operation label so the pipeline can isolate bad input. This
// file is part of package clock; see model.go for the time base.
package clock

import "fmt"

// Code enumerates the shared, stable error taxonomy.
type Code int

const (
	CodeInvalidRate Code = iota + 1
	CodeRateOverflow
	CodeNegativeDuration
	CodeMonotonicWall
	CodeBoxBounds
	CodeBoxTruncated
	CodeBoxOverflow
	CodeFragmentMissing
	CodeFragmentTruncated
	CodeSequenceWindow
	CodeTrackCycle
	CodeTrackMissing
	CodeRecoveryExhausted
	CodeSyncOverLimit
	CodeAuditMismatch
	CodeArgument
	CodeIO
	CodeInternal
)

var codeNames = [...]string{
	CodeInvalidRate:       "invalid_rate",
	CodeRateOverflow:      "rate_overflow",
	CodeNegativeDuration:  "negative_duration",
	CodeMonotonicWall:     "monotonic_wall",
	CodeBoxBounds:         "box_bounds",
	CodeBoxTruncated:      "box_truncated",
	CodeBoxOverflow:       "box_overflow",
	CodeFragmentMissing:   "fragment_missing",
	CodeFragmentTruncated: "fragment_truncated",
	CodeSequenceWindow:    "sequence_window",
	CodeTrackCycle:        "track_cycle",
	CodeTrackMissing:      "track_missing",
	CodeRecoveryExhausted: "recovery_exhausted",
	CodeSyncOverLimit:     "sync_over_limit",
	CodeAuditMismatch:     "audit_mismatch",
	CodeArgument:          "argument",
	CodeIO:                "io",
	CodeInternal:          "internal",
}

// String renders a Code with a fixed token.
func (c Code) String() string {
	if c >= 1 && int(c) < len(codeNames) && codeNames[c] != "" {
		return codeNames[c]
	}
	return fmt.Sprintf("code_%d", int(c))
}

// Error is the shared error model: a stable code, the first offending byte
// (or -1) and an operation label so the pipeline can isolate bad input.
type Error struct {
	Code    Code
	Op      string
	Offset  int64
	Detail  string
	Wrapped error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Code.String()
	if e.Op != "" {
		msg = e.Op + ": " + msg
	}
	if e.Offset >= 0 {
		msg += fmt.Sprintf(" at byte %d", e.Offset)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Unwrap exposes the wrapped cause for errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Wrapped }

// New builds a shared-model error with no byte offset.
func New(code Code, op, detail string) error {
	return &Error{Code: code, Op: op, Offset: -1, Detail: detail}
}

// At builds a shared-model error anchored at a byte offset.
func At(code Code, op string, offset int64, detail string) error {
	return &Error{Code: code, Op: op, Offset: offset, Detail: detail}
}

// Wrap augments an error with an operation label, preserving code and offset.
func Wrap(code Code, op string, offset int64, cause error) error {
	if cause == nil {
		return New(code, op, "")
	}
	if e, ok := cause.(*Error); ok {
		merged := &Error{Code: e.Code, Op: op, Offset: offset, Detail: e.Detail, Wrapped: cause}
		if offset < 0 && e.Offset >= 0 {
			merged.Offset = e.Offset
		}
		return merged
	}
	return &Error{Code: code, Op: op, Offset: offset, Wrapped: cause}
}

// CodeOf extracts the code, defaulting to CodeInternal.
func CodeOf(err error) Code {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return CodeInternal
}

// OffsetOf extracts the first bad byte, or -1 when absent.
func OffsetOf(err error) int64 {
	if e, ok := err.(*Error); ok {
		return e.Offset
	}
	return -1
}

var (
	ErrInvalidRate      = New(CodeInvalidRate, "rate", "ticks and second must be positive")
	ErrRateOverflow     = New(CodeRateOverflow, "rate", "value exceeds supported bounds")
	ErrNegativeDuration = New(CodeNegativeDuration, "duration", "negative duration")
	ErrMonotonicWall    = New(CodeMonotonicWall, "timeline", "wall clock moved backwards")
)
