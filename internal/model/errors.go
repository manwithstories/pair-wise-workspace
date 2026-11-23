// Package model defines the shared classification of failures used by every
// mediad subsystem. A failure is never a bare error: the pipeline needs to know
// whether it may isolate one frame and continue, whether the input is beyond
// recovery, or whether output verification failed.
package model

import (
	"errors"
	"fmt"
)

// Class is the recovery class of an error.
type Class uint8

const (
	// ClassNone is the zero value and means "no classified failure".
	ClassNone Class = iota
	// ClassRecoverable marks a defect confined to a single frame or packet.
	// The pipeline isolates the offending item, records it in the report and
	// keeps going; it never aborts the whole track.
	ClassRecoverable
	// ClassFatal marks an input that cannot be interpreted at all (unknown
	// container, unreadable source, no usable frames). Exit code 2.
	ClassFatal
	// ClassVerify marks a failed output check or metric threshold. Exit code 3.
	ClassVerify
)

// ExitCode maps a failure to the process exit code contract: 0 success,
// 2 unrecoverable input, 3 verification or metric failure.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch ClassOf(err) {
	case ClassFatal:
		return 2
	case ClassVerify, ClassRecoverable:
		return 3
	default:
		return 3
	}
}

// Error carries a class, the operation that failed and the coordinate of the
// affected item so terminal output can point at the exact frame.
type Error struct {
	Class  Class
	Op     string
	Track  int
	Frame  uint64
	Kind   string
	Detail string
	Err    error
}

func (e *Error) Error() string {
	loc := ""
	if e.Track != 0 {
		loc = fmt.Sprintf(" track=%d", e.Track)
	}
	if e.Frame != 0 {
		loc += fmt.Sprintf(" frame=%d", e.Frame)
	}
	kind := ""
	if e.Kind != "" {
		kind = " kind=" + e.Kind
	}
	msg := e.Op + loc + kind
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// ClassOf reports the class of err, walking wrapped errors.
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ClassNone
}

func classify(class Class, op string, track int, frame uint64, kind string, err error) *Error {
	return &Error{Class: class, Op: op, Track: track, Frame: frame, Kind: kind, Err: err}
}

// Recoverable marks a frame- or packet-level defect.
func Recoverable(op string, track int, frame uint64, kind string, err error) *Error {
	return classify(ClassRecoverable, op, track, frame, kind, err)
}

// Fatal marks an unrecoverable input failure.
func Fatal(op string, err error) *Error { return classify(ClassFatal, op, 0, 0, "", err) }

// Verify marks a failed verification step.
func Verify(op string, err error) *Error { return classify(ClassVerify, op, 0, 0, "", err) }

// VerifyAt marks a failed verification tied to one track/frame.
func VerifyAt(op string, track int, frame uint64, kind string, err error) *Error {
	return classify(ClassVerify, op, track, frame, kind, err)
}

// IsClass reports whether err carries class c.
func IsClass(err error, c Class) bool { return ClassOf(err) == c }

// ErrInterrupted signals that the pipeline stopped on purpose after a
// checkpoint commit. It is not an input or verification failure.
var ErrInterrupted = errors.New("mediad: interrupted after checkpoint")

// ErrUnfinishedOutput is returned when final outputs exist without a committed
// checkpoint. The constraint forbids overwriting unfinished output.
var ErrUnfinishedOutput = errors.New("mediad: unfinished output present")

// ErrCacheInvalid is returned (and handled) when a cache hit fails provenance
// checking. It must always force a recompute.
var ErrCacheInvalid = errors.New("mediad: cache entry failed provenance check")
