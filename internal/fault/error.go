// Package fault defines the shared recoverable-error model. Every subsystem
// reports failures as *Error so the entry point can map them to exit codes
// and so callers can locate the fault (operation + position).
package fault

import "errors"

// Code classifies a failure.
type Code int

const (
	// Unsupported means the input cannot be processed at all (exit 2):
	// truncated headers, non-integral frames, unknown layouts or configs.
	Unsupported Code = iota + 1
	// Corrupt means the input is damaged but the pipeline may recover
	// (reorder, redundancy, checkpoints).
	Corrupt
	// Unrecovered means damage survived recovery (exit 3).
	Unrecovered
)

// Error is a locatable, classified failure.
type Error struct {
	Code Code
	Op   string // subsystem operation, e.g. "wav.decode"
	Pos  int64  // byte/sample/sequence position, -1 if not applicable
	Msg  string
}

func (e *Error) Error() string {
	if e.Pos >= 0 {
		return e.Op + " @" + itoa(e.Pos) + ": " + e.Msg
	}
	return e.Op + ": " + e.Msg
}

// New builds an *Error.
func New(code Code, op string, pos int64, msg string) *Error {
	return &Error{Code: code, Op: op, Pos: pos, Msg: msg}
}

// IsCode reports whether err (or anything it wraps) is an *Error with code.
func IsCode(err error, code Code) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == code
	}
	return false
}

// ExitCode maps an error to the process exit code contract:
// 0 ok, 2 unsupported input, 3 unrecovered corruption.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		switch e.Code {
		case Unsupported:
			return 2
		case Unrecovered:
			return 3
		}
	}
	return 1
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
