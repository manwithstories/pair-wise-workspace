package media

import (
	"errors"
	"fmt"
)

// Class categorizes a failure so the pipeline can decide whether to retry,
// degrade or halt.
type Class int

const (
	// ClassRecoverable: partial results exist; downstream may proceed with
	// what was salvaged (e.g. truncated tail, damaged trailing box).
	ClassRecoverable Class = iota
	// ClassFatal: input cannot be used at all (e.g. missing moov).
	ClassFatal
	// ClassBlocked: a dependency cannot be satisfied; Dep names the first
	// blocking dependency.
	ClassBlocked
)

func (c Class) String() string {
	switch c {
	case ClassRecoverable:
		return "recoverable"
	case ClassFatal:
		return "fatal"
	case ClassBlocked:
		return "blocked"
	}
	return "unknown"
}

// Error is the shared error model. Offset is the first invalid byte offset
// when the failure is tied to a position in the input.
type Error struct {
	Class  Class
	Op     string // subsystem operation, e.g. "demux.parse"
	Offset int64  // first invalid offset, -1 when not applicable
	Dep    string // first blocking dependency, only for ClassBlocked
	Err    error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Op, e.Err)
	if e.Offset >= 0 {
		s += fmt.Sprintf(" (first invalid offset %d)", e.Offset)
	}
	if e.Dep != "" {
		s += fmt.Sprintf(" (blocked by %s)", e.Dep)
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// Recoverable builds a ClassRecoverable error at the given offset.
func Recoverable(op string, offset int64, err error) *Error {
	return &Error{Class: ClassRecoverable, Op: op, Offset: offset, Err: err}
}

// Fatal builds a ClassFatal error.
func Fatal(op string, err error) *Error {
	return &Error{Class: ClassFatal, Op: op, Offset: -1, Err: err}
}

// Blocked builds a ClassBlocked error naming the first blocking dependency.
func Blocked(op, dep string, err error) *Error {
	return &Error{Class: ClassBlocked, Op: op, Offset: -1, Dep: dep, Err: err}
}

// ClassOf extracts the Class of err, defaulting to ClassFatal.
func ClassOf(err error) Class {
	var me *Error
	if errors.As(err, &me) {
		return me.Class
	}
	return ClassFatal
}

// FirstBlockingDep walks err and returns the first named blocking
// dependency, or "" if the chain is not a blocked-class failure.
func FirstBlockingDep(err error) string {
	var me *Error
	if errors.As(err, &me) && me.Class == ClassBlocked {
		return me.Dep
	}
	return ""
}

// Sentinel classification errors.
var (
	ErrTruncated       = errors.New("input truncated")
	ErrBrokenBox       = errors.New("broken box")
	ErrMissingMoov     = errors.New("missing moov box")
	ErrIncompleteIndex = errors.New("incomplete sample index")
	ErrNoAudioTrack    = errors.New("no audio track")
	ErrTimestampRegr   = errors.New("timestamp regression")
)
