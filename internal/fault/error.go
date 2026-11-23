// Package fault defines the shared, offset-bearing error model used by the
// parser, repair and index subsystems.
package fault

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies a fault without hiding its underlying cause.
type Kind string

const (
	KindUnknown     Kind = "unknown"
	KindShortRead   Kind = "short_read"
	KindInvalidSize Kind = "invalid_size"
	KindOverflow    Kind = "overflow"
	KindOutOfBounds Kind = "out_of_bounds"
	KindPath        Kind = "unsafe_path"
	KindNotRegular  Kind = "not_regular"
	KindMalformed   Kind = "malformed_box"
	KindMissingBox  Kind = "missing_box"
	KindSequence    Kind = "sequence"
	KindTimeline    Kind = "timeline"
	KindIndex       Kind = "index"
	KindCheckpoint  Kind = "checkpoint"
	KindOutput      Kind = "output"
)

// Error carries the input segment, the four-character box type and the raw
// byte offset at which a fault was detected.
type Error struct {
	Segment string
	BoxType string
	Offset  int64
	Kind    Kind
	Detail  string
	Err     error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString("kind=")
	b.WriteString(string(e.Kind))
	if e.Segment != "" {
		b.WriteString(" segment=")
		b.WriteString(e.Segment)
	}
	if e.BoxType != "" {
		b.WriteString(" box=")
		b.WriteString(e.BoxType)
	}
	b.WriteString(fmt.Sprintf(" offset=%d", e.Offset))
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

// Unwrap supports errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// New constructs an offset-bearing fault.
func New(kind Kind, segment, boxType string, offset int64, detail string) *Error {
	return &Error{Kind: kind, Segment: segment, BoxType: boxType, Offset: offset, Detail: detail}
}

// Wrap constructs a fault with an underlying cause.
func Wrap(err error, kind Kind, segment, boxType string, offset int64, detail string) *Error {
	return &Error{Kind: kind, Segment: segment, BoxType: boxType, Offset: offset, Detail: detail, Err: err}
}

// As extracts a *fault.Error from err.
func As(err error) (*Error, bool) {
	var fe *Error
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

// OffsetOf returns the first raw offset found in an error chain.
func OffsetOf(err error) (int64, bool) {
	if fe, ok := As(err); ok {
		return fe.Offset, true
	}
	return 0, false
}

// IsKind reports whether err contains a fault of the requested kind.
func IsKind(err error, kind Kind) bool {
	fe, ok := As(err)
	return ok && fe.Kind == kind
}

// Join attaches context without losing typed faults.
func Join(errs ...error) error { return errors.Join(errs...) }
