// Package diag defines the structured error model shared by every pipeline
// stage. Every error carries the originating source, a byte offset into that
// source (when known), the pipeline phase that produced it and whether the
// caller may recover by skipping the offending record instead of aborting.
package diag

import (
	"errors"
	"fmt"
	"strings"
)

// Phase identifies the pipeline stage that emitted an error.
type Phase string

const (
	PhaseRead    Phase = "read"
	PhaseParse   Phase = "parse"
	PhaseConvert Phase = "convert"
	PhaseMerge   Phase = "merge"
	PhaseIndex   Phase = "index"
	PhaseQueue   Phase = "queue"
	PhaseRender  Phase = "render"
	PhaseVerify  Phase = "verify"
	PhaseWrite   Phase = "write"
)

// OffsetUnknown marks errors that are not anchored at a byte offset.
const OffsetUnknown int64 = -1

// Sentinels so callers can classify failures with errors.Is regardless of the
// wrapping source/offset metadata.
var (
	ErrInvalid   = errors.New("invalid record")
	ErrTruncated = errors.New("truncated input")
	ErrOverflow  = errors.New("value out of allowed range")
	ErrChecksum  = errors.New("hash verification failed")
)

// Error is the single error type used across the archive pipeline.
type Error struct {
	Phase       Phase
	Source      string // file or stream name, empty when not yet known
	Offset      int64  // byte offset into Source; OffsetUnknown when unavailable
	Op          string // short operation label, e.g. "smpte.parse"
	Message     string
	Recoverable bool  // true when skipping the record keeps the run viable
	Cause       error // wrapped sentinel or underlying error
}

// New builds a structured error without a wrapped cause.
func New(phase Phase, source, op, message string, recoverable bool) *Error {
	return &Error{Phase: phase, Source: source, Offset: OffsetUnknown,
		Op: op, Message: message, Recoverable: recoverable, Cause: ErrInvalid}
}

// Wrap builds a structured error around an underlying error.
func Wrap(phase Phase, source, op string, recoverable bool, cause error) *Error {
	return &Error{Phase: phase, Source: source, Offset: OffsetUnknown,
		Op: op, Message: cause.Error(), Recoverable: recoverable, Cause: cause}
}

// At returns a copy of the error anchored at a concrete source and byte offset.
// The receiver is left untouched so partially-annotated errors stay reusable.
func (e *Error) At(source string, offset int64) *Error {
	clone := *e
	if source != "" {
		clone.Source = source
	}
	clone.Offset = offset
	return &clone
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Phase))
	b.WriteByte('/')
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(e.Message)
	if e.Source != "" {
		fmt.Fprintf(&b, " [source=%s", e.Source)
		if e.Offset != OffsetUnknown {
			fmt.Fprintf(&b, " offset=%d", e.Offset)
		}
		b.WriteByte(']')
	} else if e.Offset != OffsetUnknown {
		fmt.Fprintf(&b, " [offset=%d]", e.Offset)
	}
	if e.Recoverable {
		b.WriteString(" (recoverable)")
	}
	return b.String()
}

// Unwrap exposes the wrapped cause for errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// Collector accumulates structured errors while a stream keeps running. Once
// the optional limit is reached Add returns false so callers can fail fast.
type Collector struct {
	errs  []*Error
	limit int
}

// NewCollector creates a collector; limit <= 0 means unbounded.
func NewCollector(limit int) *Collector { return &Collector{limit: limit} }

// Add records an error and reports whether the collector still has capacity.
func (c *Collector) Add(e *Error) bool {
	c.errs = append(c.errs, e)
	return c.limit <= 0 || len(c.errs) < c.limit
}

// All returns the recorded errors in arrival order.
func (c *Collector) All() []*Error { return c.errs }

// Len is the number of recorded errors.
func (c *Collector) Len() int { return len(c.errs) }

// Recoverable counts errors that permit the run to continue.
func (c *Collector) Recoverable() int {
	n := 0
	for _, e := range c.errs {
		if e.Recoverable {
			n++
		}
	}
	return n
}

// Fatal returns the first unrecoverable error, or nil.
func (c *Collector) Fatal() *Error {
	for _, e := range c.errs {
		if !e.Recoverable {
			return e
		}
	}
	return nil
}
