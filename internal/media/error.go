package media

import (
	"errors"
	"fmt"
	"strings"
)

// Class is the severity/exit tier of a MediaError. Every failure that reaches
// the command adapter is classified so that the process can pick a stable exit
// code without inspecting message text.
type Class int

const (
	// ClassInternal is an unexpected implementation failure. Exit code 1.
	ClassInternal Class = iota
	// ClassInput means parameters, decode structure or bounds are invalid. Exit code 2.
	ClassInput
	// ClassIntegrity means a produced or received artifact failed validation and
	// any uncommitted work was rolled back. Exit code 3.
	ClassIntegrity
	// ClassInterrupted means the run stopped on purpose at a durable boundary and
	// can be resumed. Exit code 4.
	ClassInterrupted
)

func (c Class) ExitCode() int {
	switch c {
	case ClassInput:
		return 2
	case ClassIntegrity:
		return 3
	case ClassInterrupted:
		return 4
	default:
		return 1
	}
}

func (c Class) String() string {
	switch c {
	case ClassInput:
		return "input"
	case ClassIntegrity:
		return "integrity"
	case ClassInterrupted:
		return "interrupted"
	default:
		return "internal"
	}
}

// Error is the single error type shared by all media subsystems. Code is a
// stable machine-readable identifier; Stage/Path/Field pinpoint the failure so
// the terminal output is actionable.
type Error struct {
	Class  Class
	Code   string
	Stage  string
	Path   string
	Field  string
	Detail string
	Cause  error
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Stage != "" {
		b.WriteString("[" + e.Stage + "] ")
	}
	b.WriteString(e.Code)
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	var loc []string
	if e.Path != "" {
		loc = append(loc, "path="+e.Path)
	}
	if e.Field != "" {
		loc = append(loc, "field="+e.Field)
	}
	if len(loc) > 0 {
		b.WriteString(" (" + strings.Join(loc, ", ") + ")")
	}
	if e.Cause != nil {
		b.WriteString(": " + e.Cause.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

// WithStage returns the same error annotated with the pipeline stage.
func (e *Error) WithStage(stage string) *Error {
	if e.Stage == "" {
		e.Stage = stage
	}
	return e
}

// WithPath returns the same error annotated with the offending artifact path.
func (e *Error) WithPath(path string) *Error {
	if e.Path == "" {
		e.Path = path
	}
	return e
}

// WithField returns the same error annotated with the offending JSON field.
func (e *Error) WithField(field string) *Error {
	if e.Field == "" {
		e.Field = field
	}
	return e
}

func newError(class Class, code, format string, args ...any) *Error {
	return &Error{Class: class, Code: code, Detail: fmt.Sprintf(format, args...)}
}

// E builds an MediaError of any class.
func E(class Class, code, format string, args ...any) *Error {
	return newError(class, code, format, args...)
}

// Input builds a class-2 structure/parameter error.
func Input(code, format string, args ...any) *Error {
	return newError(ClassInput, code, format, args...)
}

// Integrity builds a class-3 validation error.
func Integrity(code, format string, args ...any) *Error {
	return newError(ClassIntegrity, code, format, args...)
}

// Interrupted builds a class-4 resumable interruption.
func Interrupted(code, format string, args ...any) *Error {
	return newError(ClassInterrupted, code, format, args...)
}

// Wrap classifies an underlying error while keeping its cause.
func Wrap(err error, class Class, code, format string, args ...any) *Error {
	e := newError(class, code, format, args...)
	e.Cause = err
	return e
}

// AsError extracts a *MediaError from an error chain.
func AsError(err error) (*Error, bool) {
	var me *Error
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// ExitCode maps any error to the documented process exit code.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if me, ok := AsError(err); ok {
		return me.Class.ExitCode()
	}
	return 1
}
