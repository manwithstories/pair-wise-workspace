package clock

import (
	"errors"
	"fmt"
)

// ErrorKind classifies every error raised inside medpipe so the terminal
// adapter can map it to a stable process exit code.
type ErrorKind int

const (
	// KindOther covers failures without a dedicated contract (IO errors ...).
	KindOther ErrorKind = iota + 1
	// KindArgument is a bad command line / parameter error -> exit 2.
	KindArgument
	// KindCorrupt is unrecoverable media corruption -> exit 3.
	KindCorrupt
	// KindInconsistent is a checksum, checkpoint or IO mismatch -> exit 4.
	KindInconsistent
	// KindUnsupported is a track layout the pipeline cannot map. The track
	// is stopped, its error context is preserved.
	KindUnsupported
	// KindCanceled is a pipeline cancellation or write interruption.
	KindCanceled
)

// ExitCode maps an error kind to the contract's process exit code.
func (k ErrorKind) ExitCode() int {
	switch k {
	case KindArgument:
		return 2
	case KindCorrupt, KindUnsupported:
		return 3
	case KindInconsistent:
		return 4
	case KindCanceled:
		return 130
	default:
		return 1
	}
}

// Error is the single error model shared by scanning, slicing and audio
// modules. Byte offsets use -1 when the failure has no file position.
type Error struct {
	Kind      ErrorKind
	Op        string
	TrackID   uint32
	Timestamp *Tick
	Offset    int64
	Detail    string
	Err       error
}

// E builds a pipeline error. Functional options keep call sites short.
func E(kind ErrorKind, op string, opts ...ErrOpt) *Error {
	e := &Error{Kind: kind, Op: op, Offset: -1}
	for _, o := range opts {
		o(e)
	}
	return e
}

// ErrOpt configures an Error at construction.
type ErrOpt func(*Error)

// WithTrack attaches the offending track id.
func WithTrack(id uint32) ErrOpt { return func(e *Error) { e.TrackID = id } }

// WithTick attaches the media timestamp where the failure was observed.
func WithTick(t Tick) ErrOpt { return func(e *Error) { e.Timestamp = &t } }

// WithOffset attaches the byte offset of the first anomalous byte.
func WithOffset(off int64) ErrOpt { return func(e *Error) { e.Offset = off } }

// WithDetail attaches a human readable detail string.
func WithDetail(f string, args ...any) ErrOpt {
	return func(e *Error) { e.Detail = fmt.Sprintf(f, args...) }
}

// WithWrap attaches a cause.
func WithWrap(err error) ErrOpt { return func(e *Error) { e.Err = err } }

// Error renders the full context: op, track, timestamp, byte offset.
func (e *Error) Error() string {
	s := e.Op + ": "
	if e.Detail != "" {
		s += e.Detail
	}
	if e.TrackID != 0 {
		s += fmt.Sprintf(" [track=%d]", e.TrackID)
	}
	if e.Timestamp != nil {
		s += fmt.Sprintf(" [ts=%d/%d]", e.Timestamp.Value, e.Timestamp.Timescale)
	}
	if e.Offset >= 0 {
		s += fmt.Sprintf(" [byte=%d]", e.Offset)
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap exposes the cause for errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.Err }

// ExitCode resolves the contract exit code of any error, defaulting to 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Kind.ExitCode()
	}
	return 1
}

// IsKind reports whether err carries the given error kind.
func IsKind(err error, kind ErrorKind) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Kind == kind
}
