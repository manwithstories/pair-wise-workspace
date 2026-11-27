// Package fault defines the unified failure vocabulary shared by parsing,
// transform, pipeline and verification. Every fault carries the original byte
// offset where the problem was observed; that offset is written once at
// construction and cannot be rewritten afterwards, so downstream stages can
// always map a failure back to the exact position in the source file.
package fault

import (
	"errors"
	"fmt"
)

// Kind classifies a fault.
type Kind int

const (
	// Truncated means the input ended inside a record or index page.
	Truncated Kind = iota + 1
	// CRC means a record checksum did not match its payload.
	CRC
	// Length means a declared length is zero, too large or inconsistent.
	Length
	// Index means the segment index is missing, truncated or stale.
	Index
	// Boundary means a timeline clip boundary fell inside a record.
	Boundary
	// Limit means a hard limit (record size, track count, scan budget) was hit.
	Limit
	// Canceled means the caller aborted the operation.
	Canceled
	// Checkpoint means persisted resume state was unusable.
	Checkpoint
	// Verify means an output did not match its expected digest or shape.
	Verify
)

func (k Kind) String() string {
	switch k {
	case Truncated:
		return "truncated"
	case CRC:
		return "crc"
	case Length:
		return "length"
	case Index:
		return "index"
	case Boundary:
		return "boundary"
	case Limit:
		return "limit"
	case Canceled:
		return "canceled"
	case Checkpoint:
		return "checkpoint"
	case Verify:
		return "verify"
	default:
		return "unknown"
	}
}

// Fault is an immutable error value anchored to a source byte offset.
type Fault struct {
	kind    Kind
	offset  int64
	length  int64
	track   uint8
	detail  string
	covered int64

	// wrapped holds the underlying cause; it is never mutated after
	// construction so the original offset stays authoritative.
	wrapped error
}

// New builds a fault at offset. Negative offsets are rejected and clamped to
// zero so that a fault can always be reported.
func New(kind Kind, offset int64, format string, args ...any) *Fault {
	if offset < 0 {
		offset = 0
	}
	return &Fault{kind: kind, offset: offset, detail: fmt.Sprintf(format, args...)}
}

// Wrap attaches a cause to a fault built with New.
func Wrap(kind Kind, offset int64, cause error, format string, args ...any) *Fault {
	f := New(kind, offset, format, args...)
	f.wrapped = cause
	return f
}

// Kind returns the classification.
func (f *Fault) Kind() Kind { return f.kind }

// Offset returns the original byte offset in the source input.
func (f *Fault) Offset() int64 { return f.offset }

// Length returns the byte extent of the damage, when known.
func (f *Fault) Length() int64 { return f.length }

// Track returns the affected track, when known.
func (f *Fault) Track() uint8 { return f.track }

// Detail returns the human readable explanation.
func (f *Fault) Detail() string { return f.detail }

// Error renders kind, offset and detail in a stable form.
func (f *Fault) Error() string {
	return fmt.Sprintf("%s fault at offset %d: %s", f.kind, f.offset, f.detail)
}

// Unwrap exposes the cause for errors.Is and errors.As.
func (f *Fault) Unwrap() error { return f.wrapped }

// WithLength records the damaged byte extent and returns the same fault.
// It is only meaningful while the fault is being constructed by its owner.
func (f *Fault) WithLength(n int64) *Fault {
	if n > 0 {
		f.length = n
	}
	return f
}

// WithTrack records the affected track and returns the same fault.
func (f *Fault) WithTrack(t uint8) *Fault {
	f.track = t
	return f
}

// DefaultError builds a fault without a specific offset context.
func DefaultError(kind Kind, format string, args ...any) *Fault {
	return New(kind, 0, format, args...)
}

// As extracts the first fault in err's chain.
func As(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// IsKind reports whether err contains a fault of the given kind.
func IsKind(err error, kind Kind) bool {
	f, ok := As(err)
	return ok && f.kind == kind
}

// ErrInvalidInput mirrors truncation/invalid data as a sentinel class usable
// with errors.Is at command boundaries.
var (
	ErrUnrecoverable = errors.New("input unrecoverable")
	ErrVerify        = errors.New("output verification failed")
)
