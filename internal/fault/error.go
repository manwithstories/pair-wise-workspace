// Package fault defines the single error model used across the whole
// pipeline. Every error carries the track, extended sequence number and
// input byte offset it was detected at, plus whether processing may
// continue, so that checkpointing, provenance and exit codes all speak
// the same language.
package fault

import "fmt"

// Kind classifies a pipeline error.
type Kind int

const (
	// KindCorrupt marks malformed input bytes (bad RTP header, bad
	// framing, truncated record).
	KindCorrupt Kind = iota
	// KindLost marks missing packets detected via sequence gaps or
	// sender-report packet counts.
	KindLost
	// KindDrift marks a track clock that jumped beyond the allowed
	// threshold relative to the shared timeline.
	KindDrift
	// KindResource marks exceeded limits (queue, duration, output space).
	KindResource
	// KindInternal marks runtime/programming failures.
	KindInternal
)

func (k Kind) String() string {
	switch k {
	case KindCorrupt:
		return "corrupt"
	case KindLost:
		return "lost"
	case KindDrift:
		return "drift"
	case KindResource:
		return "resource"
	case KindInternal:
		return "internal"
	default:
		return "unknown"
	}
}

// Unknown is the sentinel for fields that could not be determined.
const Unknown int64 = -1

// Error locates a failure by track, extended sequence number and byte
// offset in the input stream.
type Error struct {
	Kind        Kind
	Track       int64 // track id, or Unknown
	Seq         int64 // extended sequence number, or Unknown
	Offset      int64 // byte offset in the input stream, or Unknown
	Recoverable bool  // true when processing may continue past this error
	Msg         string
	Err         error // optional wrapped cause
}

func (e *Error) Error() string {
	s := fmt.Sprintf("fault[%s] track=%d seq=%d offset=%d recoverable=%t: %s",
		e.Kind, e.Track, e.Seq, e.Offset, e.Recoverable, e.Msg)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an Error without a wrapped cause.
func New(kind Kind, track, seq, off int64, recoverable bool, msg string) *Error {
	return &Error{Kind: kind, Track: track, Seq: seq, Offset: off,
		Recoverable: recoverable, Msg: msg}
}

// Wrap builds an Error wrapping a lower-level cause.
func Wrap(kind Kind, track, seq, off int64, recoverable bool, err error, msg string) *Error {
	return &Error{Kind: kind, Track: track, Seq: seq, Offset: off,
		Recoverable: recoverable, Msg: msg, Err: err}
}

// List is an ordered collection of faults gathered during a run. It is
// bounded: once Cap faults are retained, further faults are counted in
// Dropped instead of being stored, so a flooded error path cannot grow
// memory without limit.
type List struct {
	Errs    []*Error
	Cap     int
	Dropped int
}

// Add appends err if it is non-nil.
func (l *List) Add(err error) {
	if err == nil {
		return
	}
	if l.Cap > 0 && len(l.Errs) >= l.Cap {
		l.Dropped++
		return
	}
	if fe, ok := err.(*Error); ok {
		l.Errs = append(l.Errs, fe)
		return
	}
	l.Errs = append(l.Errs, &Error{Kind: KindInternal,
		Track: Unknown, Seq: Unknown, Offset: Unknown, Msg: err.Error(), Err: err})
}

// Count returns how many faults of the given kind were recorded.
func (l *List) Count(kind Kind) int {
	n := 0
	for _, e := range l.Errs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// HasFatal reports whether any recorded fault is not recoverable.
func (l *List) HasFatal() bool {
	for _, e := range l.Errs {
		if !e.Recoverable {
			return true
		}
	}
	return false
}
