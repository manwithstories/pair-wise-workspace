// Package fault defines the error model shared by parsing, transport, timeline
// and render stages. Every error carries the track it belongs to, the byte or
// tick offset where it was detected, and whether the pipeline can continue by
// isolating the damage.
package fault

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Kind classifies a fault so callers can decide whether it is fatal.
type Kind string

const (
	// KindTruncated means a declared container length exceeds available bytes.
	KindTruncated Kind = "truncated"
	// KindOutOfRange means a sample offset/size or KLV length is invalid.
	KindOutOfRange Kind = "out-of-range"
	// KindUnsupported means the payload codec cannot be handled by this build.
	KindUnsupported Kind = "unsupported"
	// KindCorrupt means structural validation failed (bad magic, bad CRC...).
	KindCorrupt Kind = "corrupt"
	// KindLimit means an input exceeded a configured resource cap.
	KindLimit Kind = "limit"
	// KindMetadata means metadata fields conflict and were resolved by policy.
	KindMetadata Kind = "metadata-conflict"
	// KindDrift means the estimated clock drift exceeds the alignment bound.
	KindDrift Kind = "drift-exceeded"
	// KindGap means packets or samples were lost/evicted deterministically.
	KindGap Kind = "gap"
	// KindCancelled marks a run stopped by cancellation; it is resumable.
	KindCancelled Kind = "cancelled"
)

// Error is the single error type flowing through the pipeline.
type Error struct {
	Track       int    // -1 when not track-specific
	Offset      int64  // byte offset in the container or tick offset
	Kind        Kind   // classification
	Recoverable bool   // true when the caller may isolate and continue
	Detail      string // human readable description
	Err         error  // wrapped cause, may be nil
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("fault[")
	b.WriteString(string(e.Kind))
	b.WriteString("]")
	if e.Track >= 0 {
		fmt.Fprintf(&b, " track=%d", e.Track)
	}
	if e.Offset != 0 {
		fmt.Fprintf(&b, " offset=%d", e.Offset)
	}
	if e.Recoverable {
		b.WriteString(" recoverable")
	} else {
		b.WriteString(" fatal")
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

// Unwrap exposes the wrapped cause.
func (e *Error) Unwrap() error { return e.Err }

// Is matches by Kind, allowing errors.Is(err, fault.OfKind(KindTruncated)).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return t.Kind == e.Kind
}

// OfKind returns a sentinel Error for use with errors.Is.
func OfKind(k Kind) *Error { return &Error{Track: -1, Kind: k} }

// New builds a fault with formatted detail.
func New(track int, offset int64, kind Kind, recoverable bool, format string, args ...interface{}) *Error {
	return &Error{
		Track:       track,
		Offset:      offset,
		Kind:        kind,
		Recoverable: recoverable,
		Detail:      fmt.Sprintf(format, args...),
	}
}

// Wrap builds a fault around a cause.
func Wrap(track int, offset int64, kind Kind, recoverable bool, err error) *Error {
	if err == nil {
		return nil
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe
	}
	return &Error{Track: track, Offset: offset, Kind: kind, Recoverable: recoverable, Err: err}
}

// WithTrack copies a fault with a different track number.
func (e *Error) WithTrack(track int) *Error {
	cp := *e
	cp.Track = track
	return &cp
}

// WithRecoverable copies a fault with a different recoverability flag.
func (e *Error) WithRecoverable(ok bool) *Error {
	cp := *e
	cp.Recoverable = ok
	return &cp
}

// As extracts a *Error from err.
func As(err error) (*Error, bool) {
	var fe *Error
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

// Recoverable reports whether err is a recoverable fault.
func Recoverable(err error) bool {
	fe, ok := As(err)
	return ok && fe.Recoverable
}

// List aggregates faults in deterministic order (track, offset, kind, detail).
type List struct {
	Items []*Error
}

// Add appends a fault, ignoring nil.
func (l *List) Add(err error) {
	if err == nil {
		return
	}
	if fe, ok := As(err); ok {
		l.Items = append(l.Items, fe)
		return
	}
	l.Items = append(l.Items, &Error{Track: -1, Kind: KindCorrupt, Detail: err.Error(), Err: err})
}

// Len returns the number of faults.
func (l *List) Len() int { return len(l.Items) }

// RecoverableCount returns how many faults are marked recoverable.
func (l *List) RecoverableCount() int {
	n := 0
	for _, it := range l.Items {
		if it.Recoverable {
			n++
		}
	}
	return n
}

// Fatal returns the first non-recoverable fault, or nil.
func (l *List) Fatal() *Error {
	for _, it := range l.Items {
		if !it.Recoverable {
			return it
		}
	}
	return nil
}

// Sorted returns a deterministically ordered copy of the list.
func (l *List) Sorted() []*Error {
	out := append([]*Error(nil), l.Items...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		if a.Offset != b.Offset {
			return a.Offset < b.Offset
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Detail < b.Detail
	})
	return out
}

// String renders every fault on its own line.
func (l *List) String() string {
	var b strings.Builder
	for i, it := range l.Sorted() {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(it.Error())
	}
	return b.String()
}
