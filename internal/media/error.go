package media

import (
	"errors"
	"fmt"
)

// Class classifies an error so the terminal adapter can return the exit code
// required by the delivery contract, without pattern-matching message text.
type Class uint8

const (
	// ClassFatal means the input is corrupt or otherwise unusable. Exit 2.
	ClassFatal Class = 1
	// ClassUnsupported means a mapping rule or timeline horizon rejected the
	// job. Exit 3.
	ClassUnsupported Class = 2
	// ClassInterrupted means a resumable write was interrupted. Exit 4.
	ClassInterrupted Class = 3
)

func (c Class) String() string {
	switch c {
	case ClassFatal:
		return "fatal"
	case ClassUnsupported:
		return "unsupported"
	case ClassInterrupted:
		return "interrupted"
	default:
		return fmt.Sprintf("class(%d)", uint8(c))
	}
}

// ExitCode maps a class onto the CLI contract: 2 corrupt, 3 unsupported or
// beyond-horizon, 4 recoverable interruption.
func (c Class) ExitCode() int {
	switch c {
	case ClassFatal:
		return 2
	case ClassUnsupported:
		return 3
	case ClassInterrupted:
		return 4
	default:
		return 1
	}
}

// Error is the unified error returned by every mediapipe module. Offset is a
// byte offset in the source when known (-1 otherwise); Track is a track number
// when the failure belongs to one track (0 otherwise).
type Error struct {
	Op     string
	Offset int64
	Track  uint64
	Class  Class
	Detail string
	Err    error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Detail
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	} else if e.Err != nil && e.Detail != "" {
		msg = e.Detail + ": " + e.Err.Error()
	}
	if msg == "" {
		msg = "media error"
	}
	s := fmt.Sprintf("%s: %s (class=%s", e.Op, msg, e.Class)
	if e.Offset >= 0 {
		s += fmt.Sprintf(" offset=%d", e.Offset)
	}
	if e.Track != 0 {
		s += fmt.Sprintf(" track=%d", e.Track)
	}
	return s + ")"
}

func (e *Error) Unwrap() error { return e.Err }

// NewError builds an unclassified-position error. Offset defaults to -1.
func NewError(op string, class Class, err error) *Error {
	return &Error{Op: op, Offset: -1, Class: class, Err: err}
}

// At builds an error carrying a concrete source byte offset.
func At(op string, offset int64, class Class, format string, args ...any) *Error {
	return &Error{Op: op, Offset: offset, Class: class, Detail: fmt.Sprintf(format, args...)}
}

// AtTrack builds an error carrying both offset and track number.
func AtTrack(op string, offset int64, track uint64, class Class, format string, args ...any) *Error {
	return &Error{Op: op, Offset: offset, Track: track, Class: class, Detail: fmt.Sprintf(format, args...)}
}

// OnTrack attaches tracks to an existing error, preserving offset and class.
func OnTrack(err error, track uint64) error {
	if err == nil {
		return nil
	}
	var me *Error
	if errors.As(err, &me) {
		cp := *me
		cp.Track = track
		return &cp
	}
	return &Error{Op: "media", Offset: -1, Track: track, Class: ClassFatal, Err: err}
}

// ClassOf extracts the classification of err, defaulting to fatal for unknown
// errors.
func ClassOf(err error) Class {
	if err == nil {
		return 0
	}
	var me *Error
	if errors.As(err, &me) {
		return me.Class
	}
	return ClassFatal
}

// ExitCode returns the terminal exit code for err.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var me *Error
	if errors.As(err, &me) {
		return me.Class.ExitCode()
	}
	return 1
}

// IsClass reports whether err belongs to class.
func IsClass(err error, class Class) bool {
	return err != nil && ClassOf(err) == class
}

// OffsetOf returns the reported byte offset and true when err carries one.
func OffsetOf(err error) (int64, bool) {
	var me *Error
	if errors.As(err, &me) && me.Offset >= 0 {
		return me.Offset, true
	}
	return 0, false
}

// WrapOffset is a small helper for parse loops: the first non-nil error wins.
func WrapOffset(op string, offset int64, class Class, err error) error {
	if err == nil {
		return nil
	}
	var me *Error
	if errors.As(err, &me) {
		return err
	}
	return &Error{Op: op, Offset: offset, Class: class, Err: err}
}
