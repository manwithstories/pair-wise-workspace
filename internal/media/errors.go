// Package media defines the shared error model used across the archive
// pipeline. Every error that crosses a subsystem boundary carries the track
// and the input offset (byte offset or sample index) so that operators can
// locate the failing input without re-running the whole batch.
package media

import "errors"

// Kind classifies failures for exit-code mapping.
type Kind int

const (
	// KindInternal is an unexpected fault (exit code 1).
	KindInternal Kind = iota
	// KindCorrupt means the input is damaged or violates the container
	// contract (exit code 2).
	KindCorrupt
	// KindUnsupported means a valid but unsupported feature (exit code 2).
	KindUnsupported
	// KindCanceled means the pipeline was canceled or the downstream went
	// away; it is a locatable, expected failure (exit code 2).
	KindCanceled
)

// Error is the shared locatable error. Offset is a byte offset for container
// parsing and a sample index for timeline repair; -1 means not applicable.
type Error struct {
	Kind   Kind
	Op     string // subsystem operation, e.g. "manifest.parse"
	Track  string // track id, empty if not track-scoped
	Offset int64  // byte offset or sample index, -1 if n/a
	Msg    string
	Err    error // wrapped cause, may be nil
}

func (e *Error) Error() string {
	s := e.Op
	if e.Track != "" {
		s += " track=" + e.Track
	}
	if e.Offset >= 0 {
		s += " offset=" + itoa(e.Offset)
	}
	s += ": " + e.Msg
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// New builds a locatable error.
func New(kind Kind, op, track string, offset int64, msg string, cause error) *Error {
	return &Error{Kind: kind, Op: op, Track: track, Offset: offset, Msg: msg, Err: cause}
}

// Corruptf is a shorthand for a corrupt-input error without a cause.
func Corruptf(op, track string, offset int64, msg string) *Error {
	return New(KindCorrupt, op, track, offset, msg, nil)
}

// ExitCode maps an error to the process exit code: corrupt/unsupported/
// canceled input maps to 2, anything else to 1.
func ExitCode(err error) int {
	var e *Error
	if errors.As(err, &e) {
		switch e.Kind {
		case KindCorrupt, KindUnsupported, KindCanceled:
			return 2
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
