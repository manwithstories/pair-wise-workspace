package core

import (
	"errors"
	"fmt"
)

// Kind classifies an error for process exit-code mapping and recovery.
type Kind int

const (
	KindNone Kind = iota
	// KindConfig is an invalid command line or processing parameter.
	KindConfig
	// KindIO is a local filesystem or stream I/O failure.
	KindIO
	// KindParse is malformed but potentially counted input data.
	KindParse
	// KindStrict is input rejected because strict mode is enabled.
	KindStrict
	// KindCheckpoint is irrecoverable checkpoint state corruption.
	KindCheckpoint
	// KindInterrupted is an injected, resumable interruption.
	KindInterrupted
)

// Error is the classified error type used across mediq package boundaries.
type Error struct {
	Kind   Kind
	Op     string
	Offset int64
	Err    error
}

func (e *Error) Error() string {
	msg := "mediq"
	if e.Op != "" {
		msg += " " + e.Op
	}
	if e.Offset >= 0 {
		msg += fmt.Sprintf(" at_byte=%d", e.Offset)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// New creates a classified error without a byte offset.
func New(kind Kind, op string, err error) *Error {
	return &Error{Kind: kind, Op: op, Offset: -1, Err: err}
}

// At creates a classified error anchored to an absolute stream offset.
func At(kind Kind, op string, offset int64, err error) *Error {
	return &Error{Kind: kind, Op: op, Offset: offset, Err: err}
}

// KindOf returns the classified kind, or KindNone for nil/unknown errors.
func KindOf(err error) Kind {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return KindNone
}

// ExitCode maps classified failures to the command contract.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch KindOf(err) {
	case KindStrict:
		return 2
	case KindInterrupted:
		return 75
	case KindConfig, KindIO, KindCheckpoint:
		return 1
	default:
		return 1
	}
}

// ExitError carries an explicit process exit status when no richer
// classification is appropriate.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCodeOf returns an explicit ExitError code if present.
func ExitCodeOf(err error) (int, bool) {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code, true
	}
	return 0, false
}
