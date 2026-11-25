package timeline

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable failure class shared by all modules.
type Code string

const (
	CodeOK                Code = "ok"
	CodeInvalidArgument   Code = "invalid_argument"
	CodeMissingDependency Code = "missing_dependency"
	CodeCycle             Code = "dependency_cycle"
	CodeOverflow          Code = "integer_overflow"
	CodeCorruptBox        Code = "corrupt_box"
	CodeNegativeDuration  Code = "negative_duration"
	CodeIllegalReference  Code = "illegal_reference"
	CodeWindowOverflow    Code = "window_overflow"
	CodeGap               Code = "sequence_gap"
	CodeIntegrity         Code = "integrity_conflict"
	CodePartial           Code = "partial_recovery"
)

// Error carries the locating information required by the acceptance tooling.
// Offset is a byte offset when the failure comes from a container or capture.
type Error struct {
	Code    Code
	Op      string
	Path    string
	Offset  int64
	TrackID uint32
	Seq     uint64
	Detail  string
	Err     error
}

func (e *Error) Error() string {
	s := string(e.Code)
	if e.Op != "" {
		s += ": " + e.Op
	}
	if e.Path != "" {
		s += " path=" + e.Path
	}
	if e.Offset >= 0 {
		s += fmt.Sprintf(" offset=%d", e.Offset)
	}
	if e.TrackID != 0 {
		s += fmt.Sprintf(" track=%d", e.TrackID)
	}
	if e.Seq != 0 {
		s += fmt.Sprintf(" seq=%d", e.Seq)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

func New(code Code, op, detail string) *Error {
	return &Error{Code: code, Op: op, Detail: detail, Offset: -1}
}

func Wrap(code Code, op string, err error) *Error {
	var te *Error
	if errors.As(err, &te) {
		if te.Code == "" || te.Code == CodeOK {
			te.Code = code
		}
		if te.Op == "" {
			te.Op = op
		}
		return te
	}
	return &Error{Code: code, Op: op, Err: err, Offset: -1}
}

func AtOffset(err error, path string, offset int64) error {
	var te *Error
	if errors.As(err, &te) {
		cp := *te
		if path != "" {
			cp.Path = path
		}
		if offset >= 0 && cp.Offset < 0 {
			cp.Offset = offset
		}
		return &cp
	}
	return &Error{Code: CodeInvalidArgument, Path: path, Offset: offset, Err: err}
}

func IsCode(err error, code Code) bool {
	var te *Error
	return errors.As(err, &te) && te.Code == code
}

// ExitCode maps shared failure classes to the archivectl process contract.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var te *Error
	if errors.As(err, &te) {
		switch te.Code {
		case CodeGap, CodeWindowOverflow, CodePartial:
			return 2
		default:
			return 1
		}
	}
	return 1
}
