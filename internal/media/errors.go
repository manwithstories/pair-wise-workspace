// Package media defines the shared time base, sample model and classified
// errors used by every subsystem of the media processing kernel.
package media

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Code is a stable, machine-readable error classification. Codes are printed
// verbatim by the CLI and are part of the demo contract.
type Code string

const (
	CodeUsage              Code = "ERR_USAGE"
	CodeIO                 Code = "ERR_IO"
	CodeLimit              Code = "ERR_LIMIT_EXCEEDED"
	CodeManifest           Code = "ERR_MANIFEST_SYNTAX"
	CodeMissingEntry       Code = "ERR_MISSING_ENTRY"
	CodeDuplicateSeq       Code = "ERR_DUPLICATE_SEQ"
	CodeInvalidDuration    Code = "ERR_INVALID_DURATION"
	CodeURIOutOfRange      Code = "ERR_URI_OUT_OF_RANGE"
	CodePathTraversal      Code = "ERR_PATH_TRAVERSAL"
	CodeUnsupported        Code = "ERR_UNSUPPORTED"
	CodeTruncatedBox       Code = "ERR_TRUNCATED_BOX"
	CodeSegmentParse       Code = "ERR_SEGMENT_PARSE"
	CodeTrackConflict      Code = "ERR_TRACK_CONFLICT"
	CodeRequiredTrack      Code = "ERR_REQUIRED_TRACK_MISSING"
	CodeNoTrack            Code = "ERR_NO_TRACK"
	CodeIntegrity          Code = "ERR_INTEGRITY"
	CodeClockDiscontinuity Code = "ERR_CLOCK_DISCONTINUITY"
)

// SourcePos identifies the origin of a value or failure. Line and Col are
// 1-based; zero means the position is unknown.
type SourcePos struct {
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	Col  int    `json:"col,omitempty"`
}

func (p SourcePos) String() string {
	if p.File == "" {
		return ":0:0"
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

// Error is a classified error carrying the operation and source location so
// malformed inputs can be reported and quarantined instead of silently skipped.
type Error struct {
	Code   Code      `json:"code"`
	Op     string    `json:"op,omitempty"`
	Pos    SourcePos `json:"pos"`
	Detail string    `json:"detail,omitempty"`
	Err    error     `json:"-"`
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Op != "" {
		b.WriteString(": ")
		b.WriteString(e.Op)
	}
	if e.Pos.File != "" {
		b.WriteString(": ")
		b.WriteString(e.Pos.String())
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Wrap builds a classified error. If err is already classified its code is
// preserved unless a more specific code is supplied.
func Wrap(code Code, op string, pos SourcePos, err error, format string, args ...any) *Error {
	e := &Error{Code: code, Op: op, Pos: pos, Err: err}
	if format != "" {
		e.Detail = fmt.Sprintf(format, args...)
	}
	var prev *Error
	if errors.As(err, &prev) {
		e.Code = prev.Code
		if e.Pos.File == "" {
			e.Pos = prev.Pos
		}
	}
	return e
}

// New builds a classified error without an underlying cause.
func New(code Code, op string, pos SourcePos, format string, args ...any) *Error {
	return &Error{Code: code, Op: op, Pos: pos, Detail: fmt.Sprintf(format, args...)}
}

// CodeOf returns the classification of err, defaulting to CodeIO.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return CodeTruncatedBox
	}
	return CodeIO
}

// Isolated reports whether the error denotes detected input damage that was
// contained by the caller. Such failures map to exit status 2.
func Isolated(err error) bool {
	switch CodeOf(err) {
	case CodeTruncatedBox, CodeSegmentParse, CodeManifest, CodeMissingEntry,
		CodeDuplicateSeq, CodeInvalidDuration, CodeURIOutOfRange, CodePathTraversal:
		return true
	}
	return false
}

// AsError extracts the classified error, if any.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
