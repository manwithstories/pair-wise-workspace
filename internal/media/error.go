package media

import (
	"errors"
	"fmt"
)

// Process exit codes shared by the CLI.
const (
	ExitOK          = 0
	ExitCorrupt     = 2
	ExitIO          = 3
	ExitInterrupted = 75
)

// Error is the common locatable error model.  Fragment is the original batch
// fragment number (mfhd sequence once available, otherwise the source file
// ordinal).  Offset is a zero-based byte offset in Path.
type Error struct {
	ExitCode int    `json:"exit_code"`
	Stage    string `json:"stage,omitempty"`
	Path     string `json:"path,omitempty"`
	Fragment uint32 `json:"fragment,omitempty"`
	Offset   int64  `json:"offset"`
	Kind     string `json:"kind,omitempty"`
	Err      error  `json:"-"`
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s", e.Kind)
	if e.Stage != "" {
		s += " stage=" + e.Stage
	}
	if e.Fragment != 0 {
		s += fmt.Sprintf(" fragment=%d", e.Fragment)
	}
	if e.Offset >= 0 {
		s += fmt.Sprintf(" offset=%d", e.Offset)
	}
	if e.Path != "" {
		s += " path=" + e.Path
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// New creates a located shared error.  Use offset -1 when no byte location is
// meaningful.
func New(exitCode int, stage, kind, path string, fragment uint32, offset int64, format string, args ...any) *Error {
	var cause error
	if format != "" {
		cause = fmt.Errorf(format, args...)
	}
	return &Error{
		ExitCode: exitCode,
		Stage:    stage,
		Kind:     kind,
		Path:     path,
		Fragment: fragment,
		Offset:   offset,
		Err:      cause,
	}
}

// Corrupt is the common constructor for input corruption.
func Corrupt(stage, kind, path string, fragment uint32, offset int64, format string, args ...any) *Error {
	return New(ExitCorrupt, stage, kind, path, fragment, offset, format, args...)
}

// IO is the common constructor for filesystem/WAL failures.
func IO(stage, path string, fragment uint32, offset int64, err error) *Error {
	return &Error{ExitCode: ExitIO, Stage: stage, Kind: "io_error", Path: path, Fragment: fragment, Offset: offset, Err: err}
}

// Interrupted reports deliberate fault injection or an interrupt-like abort.
func Interrupted(stage string) *Error {
	return New(ExitInterrupted, stage, "interrupted", "", 0, -1, "fault injection after %s", stage)
}

// AsError finds the shared error in err's wrap chain.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// ExitCodeOf maps any error to a process exit code.
func ExitCodeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	if e, ok := AsError(err); ok {
		return e.ExitCode
	}
	return 1
}

// Origin walks to the deepest shared Error and returns its location.  Wrappers
// created by higher layers therefore retain the original fragment/offset.
func Origin(err error) (fragment uint32, offset int64, path string) {
	offset = -1
	var cur *Error
	for {
		e, ok := AsError(err)
		if !ok {
			break
		}
		cur = e
		err = e.Err
	}
	if cur == nil {
		return 0, -1, ""
	}
	return cur.Fragment, cur.Offset, cur.Path
}

// Wrap preserves an existing shared error's exit code and origin while adding a
// stage/context message.  Non-shared errors become an I/O error only when
// ioCode is true, otherwise a corrupt error.
func Wrap(err error, stage string) error {
	if err == nil {
		return nil
	}
	if e, ok := AsError(err); ok {
		return &Error{ExitCode: e.ExitCode, Stage: stage, Kind: e.Kind, Err: err, Offset: -1}
	}
	return &Error{ExitCode: ExitCorrupt, Stage: stage, Kind: "wrapped_error", Err: err, Offset: -1}
}
