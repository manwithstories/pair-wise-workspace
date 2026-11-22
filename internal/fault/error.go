// Package fault defines the single error model shared by all subsystems.
// Every error carries the input byte offset, track id and processing stage
// where it arose, so operators can locate damage without grepping logs.
package fault

import (
	"errors"
	"fmt"
)

// Kind classifies the failure and maps to a process exit code in the CLI.
type Kind int

const (
	KindUnknown Kind = iota
	// KindCorruptInput: malformed bitstream/index; exit code 2.
	KindCorruptInput
	// KindChecksum: source/input/content digest mismatch; exit code 3.
	KindChecksum
	// KindIO: filesystem, checkpoint or recovery failure; exit code 4.
	KindIO
	// KindCanceled: the pipeline was shut down by context cancellation.
	KindCanceled
	// KindLimit: configured size/path/memory limit was exceeded.
	KindLimit
)

// Error is the only structured error type returned by mediapack packages.
type Error struct {
	Kind   Kind
	Stage  string // e.g. "parse", "index", "rtp", "mux", "cache", "checkpoint"
	Track  uint32 // 0 means "not track specific"
	Offset int64  // byte offset inside the offending input; -1 when unknown
	Msg    string
	Err    error
}

func (e *Error) Error() string {
	loc := "offset=-"
	if e.Offset >= 0 {
		loc = fmt.Sprintf("offset=%d", e.Offset)
	}
	s := fmt.Sprintf("fault[%s] stage=%s track=%d %s: %s", e.Kind, e.Stage, e.Track, loc, e.Msg)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an Error with an unknown offset.
func New(kind Kind, stage, msg string) *Error {
	return &Error{Kind: kind, Stage: stage, Offset: -1, Msg: msg}
}

// At builds an Error pinned to an input byte offset.
func At(kind Kind, stage string, offset int64, msg string) *Error {
	return &Error{Kind: kind, Stage: stage, Offset: offset, Msg: msg}
}

// WithTrack annotates an existing Error with the active track id.
func WithTrack(err error, track uint32) error {
	var fe *Error
	if errors.As(err, &fe) {
		cp := *fe
		cp.Track = track
		return &cp
	}
	return err
}

// Wrap nests a lower-level error without replacing classification.
func Wrap(err error, stage string) error {
	if err == nil {
		return nil
	}
	var fe *Error
	if errors.As(err, &fe) {
		cp := *fe
		cp.Stage = stage + "/" + cp.Stage
		cp.Err = err
		return &cp
	}
	return &Error{Kind: KindIO, Stage: stage, Offset: -1, Msg: err.Error(), Err: err}
}

// As extracts a *Error from any wrapped error chain.
func As(err error) (*Error, bool) {
	var fe *Error
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

func (k Kind) String() string {
	switch k {
	case KindCorruptInput:
		return "corrupt_input"
	case KindChecksum:
		return "checksum"
	case KindIO:
		return "io"
	case KindCanceled:
		return "canceled"
	case KindLimit:
		return "limit"
	default:
		return "unknown"
	}
}
