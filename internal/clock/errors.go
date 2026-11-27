// Package clock defines the shared time base and error model used by every
// tsqc subsystem. All timestamps cross subsystem boundaries as signed
// 27 MHz tick counts so that PCR, PTS and DTS can be compared without
// floating point rounding.
package clock

import (
	"errors"
	"fmt"
)

// Code is a stable machine readable error class.
type Code string

const (
	CodeInvalidArgument     Code = "invalid_argument"
	CodePCRNonMonotonic     Code = "pcr_non_monotonic"
	CodeTimestampRegression Code = "timestamp_regression"
	CodeSyncDrift           Code = "sync_drift"
	CodeUnrecoverable       Code = "unrecoverable"
	CodeSequence            Code = "rtp_sequence"
	CodeGap                 Code = "rtp_gap"
	CodeXORRecovery         Code = "xor_recovery"
	CodeTSInvalidSync       Code = "ts_invalid_sync"
	CodeTSContinuity        Code = "ts_continuity"
	CodePSIInvalid          Code = "psi_invalid"
	CodePSIVersion          Code = "psi_version"
	CodeTruncated           Code = "truncated"
	CodeCheckpoint          Code = "checkpoint"
	CodeDigestMismatch      Code = "digest_mismatch"
	CodeCacheMismatch       Code = "cache_mismatch"
)

// Error is the shared error model. Recoverable is false when the stream can
// no longer be made playable without user intervention.
type Error struct {
	Code        Code
	Op          string
	Message     string
	Recoverable bool
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Op == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, e.Op, e.Message)
}

// NewError builds an error with an explicit recoverability flag.
func NewError(code Code, op string, recoverable bool, format string, args ...any) *Error {
	return &Error{Code: code, Op: op, Recoverable: recoverable, Message: fmt.Sprintf(format, args...)}
}

// RecoverableError marks a condition the pipeline can route around.
func RecoverableError(code Code, op, format string, args ...any) *Error {
	return NewError(code, op, true, format, args...)
}

// FatalError marks a condition that makes the stream unplayable.
func FatalError(code Code, op, format string, args ...any) *Error {
	return NewError(code, op, false, format, args...)
}

// IsCode reports whether err unwraps to an Error with the given code.
func IsCode(err error, code Code) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == code
	}
	return false
}

// IsRecoverable reports whether err carries the recoverable marker.
func IsRecoverable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Recoverable
	}
	return false
}
