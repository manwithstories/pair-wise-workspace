// Package media provides the shared rational time base and the cross-stage
// fault/error model used by meta, transport, audio, transaction and verify.
package media

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable cross-stage error code.
type Code string

const (
	CodeOK               Code = ""
	CodeInvalidInput     Code = "invalid_input"
	CodeParseError       Code = "parse_error"
	CodeInvalidTimebase  Code = "invalid_timebase"
	CodeOverflow         Code = "overflow"
	CodeBackwardRange    Code = "backward_range"
	CodeInvalidRotation  Code = "invalid_rotation"
	CodeInvalidCrop      Code = "invalid_crop"
	CodeUnknownColor     Code = "unknown_color"
	CodeBufferLimit      Code = "buffer_limit"
	CodeGap              Code = "gap"
	CodeUnrecoverableGap Code = "unrecoverable_gap"
	CodeLatePacket       Code = "late_packet"
	CodeEmptyChannel     Code = "empty_channel"
	CodeClipping         Code = "clipping"
	CodeIncompleteRange  Code = "incomplete_range"
	CodeChecksumMismatch Code = "checksum_mismatch"
	CodeCommitFailed     Code = "commit_failed"
	CodeWALCorrupt       Code = "wal_corrupt"
	CodeIntegrity        Code = "integrity_unrecoverable"
	CodeInterrupted      Code = "interrupted"
)

// TimeRange is an exact rational [Start, End) interval on a track timeline.
type TimeRange struct {
	Start Rational `json:"start"`
	End   Rational `json:"end"`
}

// String renders the interval as "start>end" using exact rationals.
func (r TimeRange) String() string {
	return fmt.Sprintf("%s..%s", r.Start, r.End)
}

// Fault is the error type that crosses every pipeline stage. It carries the
// responsible track, the affected time interval and whether retrying (or
// resuming) can make progress.
type Fault struct {
	Code      Code       `json:"code"`
	Message   string     `json:"message"`
	TrackID   int64      `json:"track_id"`
	HaveTrack bool       `json:"have_track"`
	Range     *TimeRange `json:"time_range,omitempty"`
	Retryable bool       `json:"retryable"`
	Err       error      `json:"-"`
}

// defaultRetryable records which codes are worth a retry/resume.
func defaultRetryable(code Code) bool {
	switch code {
	case CodeGap, CodeLatePacket, CodeInterrupted, CodeCommitFailed:
		return true
	default:
		return false
	}
}

// NewFault constructs a Fault with the default retryability for its code.
func NewFault(code Code, format string, args ...any) *Fault {
	return &Fault{
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
		Retryable: defaultRetryable(code),
	}
}

// WithTrack attaches a track id and returns the fault for chaining.
func (f *Fault) WithTrack(id int64) *Fault {
	f.TrackID = id
	f.HaveTrack = true
	return f
}

// WithRange attaches an exact time interval and returns the fault.
func (f *Fault) WithRange(start, end Rational) *Fault {
	f.Range = &TimeRange{Start: start, End: end}
	return f
}

// WithRetryable overrides the retryable flag.
func (f *Fault) WithRetryable(v bool) *Fault {
	f.Retryable = v
	return f
}

// Wrap keeps the causal error for errors.Is/As and diagnostics.
func (f *Fault) Wrap(err error) *Fault {
	f.Err = err
	return f
}

func (f *Fault) Error() string {
	if f == nil {
		return ""
	}
	s := fmt.Sprintf("%s: %s", f.Code, f.Message)
	if f.HaveTrack {
		s += fmt.Sprintf(" (track_id=%d)", f.TrackID)
	}
	if f.Range != nil {
		s += fmt.Sprintf(" (time_range=%s)", f.Range)
	}
	if f.Err != nil {
		s += ": " + f.Err.Error()
	}
	return s
}

// Unwrap exposes the wrapped cause.
func (f *Fault) Unwrap() error { return f.Err }

// Is lets errors.Is compare faults by code.
func (f *Fault) Is(target error) bool {
	t, ok := target.(*Fault)
	return ok && t.Code == f.Code
}

// AsFault extracts a *Fault from an arbitrary error chain.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// ExitCode maps pipeline errors to the CLI contract:
// 0 ok, 2 invalid input, 3 unrecoverable integrity, 75 injected interrupt.
// Unexpected errors map to 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var f *Fault
	if errors.As(err, &f) {
		return f.ExitCode()
	}
	return 1
}

// ExitCode maps a single fault to its process exit code.
func (f *Fault) ExitCode() int {
	if f == nil {
		return 0
	}
	switch f.Code {
	case CodeInterrupted:
		return 75
	case CodeIntegrity, CodeChecksumMismatch, CodeWALCorrupt:
		return 3
	case CodeParseError, CodeInvalidInput, CodeInvalidTimebase, CodeOverflow,
		CodeBackwardRange, CodeInvalidRotation, CodeInvalidCrop, CodeUnknownColor,
		CodeBufferLimit, CodeUnrecoverableGap, CodeEmptyChannel, CodeClipping,
		CodeIncompleteRange:
		return 2
	case CodeCommitFailed:
		// A failed commit is not safely recoverable as input; surface integrity.
		return 3
	default:
		return 1
	}
}
