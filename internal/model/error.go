package model

import (
	"errors"
	"fmt"
)

// ErrCode classifies pipeline errors; it maps to process exit codes:
// 1 = I/O or internal error, 2 = invalid manifest or unrecoverable media,
// 3 = unquarantined boundary deviation in strict mode.
type ErrCode int

const (
	CodeIO ErrCode = iota + 1
	CodeManifest
	CodeUnrecoverable
	CodeBoundary
)

// ExitCode maps an error code to the process exit code.
func (c ErrCode) ExitCode() int {
	switch c {
	case CodeManifest, CodeUnrecoverable:
		return 2
	case CodeBoundary:
		return 3
	default:
		return 1
	}
}

// Error is the unified error type carried through the pipeline.
type Error struct {
	Code ErrCode
	Op   string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %v", e.Op, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// New wraps err in an *Error with the given code and operation.
func New(code ErrCode, op string, err error) *Error {
	return &Error{Code: code, Op: op, Err: err}
}

// Newf formats a new *Error.
func Newf(code ErrCode, op, format string, args ...interface{}) *Error {
	return &Error{Code: code, Op: op, Err: fmt.Errorf(format, args...)}
}

// HasCode reports whether err (or any error it wraps) is an *Error with code.
func HasCode(err error, code ErrCode) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == code
	}
	return false
}

// ExitCodeOf resolves the process exit code for err (0 when nil).
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code.ExitCode()
	}
	return 1
}
