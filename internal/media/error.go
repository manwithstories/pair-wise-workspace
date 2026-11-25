// Package media contains the shared rational time base and cross-module error
// contract used by manifest parsers, RTP convergence, repair, and indexing.
package media

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable failure category.
type Code string

const (
	CodeOK               Code = "ok"
	CodeInvalidArgument  Code = "invalid_argument"
	CodeMalformed        Code = "malformed_input"
	CodeMissingField     Code = "missing_field"
	CodeTruncatedBox     Code = "truncated_box"
	CodePacketConflict   Code = "packet_conflict"
	CodeUnrepairedGap    Code = "unrepaired_gap"
	CodeBlockedDependency Code = "blocked_dependency"
	CodeNotFound         Code = "not_found"
	CodeOutOfRange       Code = "out_of_range"
	CodePathViolation    Code = "path_violation"
	CodeTooLarge         Code = "too_large"
	CodeLocalIO          Code = "local_io"
	CodeInternal         Code = "internal"
)

// Error is the structured error type exchanged across subsystem boundaries.
type Error struct {
	Code   Code   `json:"code"`
	Module string `json:"module"`
	Field  string `json:"field,omitempty"`
	Index  int    `json:"index,omitempty"`
	Detail string `json:"detail,omitempty"`
	Err    error  `json:"-"`
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s/%s", e.Module, e.Code)
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.Index != 0 {
		msg += fmt.Sprintf(" index=%d", e.Index)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// NewError constructs a structured error.
func NewError(code Code, module, field, detail string) *Error {
	return &Error{Code: code, Module: module, Field: field, Detail: detail}
}

// Wrap attaches a cause while preserving structured fields.
func Wrap(err error, code Code, module, field, detail string) *Error {
	return &Error{Code: code, Module: module, Field: field, Detail: detail, Err: err}
}

// AsError finds an *Error in err's chain.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// ExitCode maps the contract to the process exit codes in the specification.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var me *Error
	if !errors.As(err, &me) {
		return 4
	}
	switch me.Code {
	case CodeBlockedDependency:
		return 3
	case CodeMalformed, CodeMissingField, CodeTruncatedBox, CodePacketConflict, CodeUnrepairedGap:
		return 2
	default:
		return 4
	}
}
