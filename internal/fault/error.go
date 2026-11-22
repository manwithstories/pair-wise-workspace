// Package fault defines the classified, serializable error model shared by
// every pipeline stage. Errors carry enough context (track, sequence
// number, raw timestamp, byte offset) for on-call staff to locate defects
// without re-running the pipeline.
package fault

import (
	"encoding/json"
	"fmt"
)

// Category classifies pipeline errors for triage and metrics.
type Category string

const (
	CatParse      Category = "parse"      // malformed input (truncated box, bad length)
	CatTiming     Category = "timing"     // negative duration, overflow, non-monotonic
	CatLoss       Category = "loss"       // sequence gaps / burst loss
	CatAlign      Category = "align"      // rendition alignment failures
	CatVersion    Category = "version"    // parser major-version incompatibility
	CatCheckpoint Category = "checkpoint" // checkpoint save/restore failures
)

// Error is a classified, JSON-serializable pipeline error.
type Error struct {
	Category Category `json:"category"`
	Track    string   `json:"track,omitempty"`
	Seq      uint32   `json:"seq,omitempty"`
	HasSeq   bool     `json:"has_seq,omitempty"`
	RawTime  int64    `json:"raw_time_ns,omitempty"`
	Offset   int64    `json:"offset,omitempty"`
	Reason   string   `json:"reason"`
}

func (e *Error) Error() string {
	s := fmt.Sprintf("[%s] %s", e.Category, e.Reason)
	if e.Track != "" {
		s += fmt.Sprintf(" track=%s", e.Track)
	}
	if e.HasSeq {
		s += fmt.Sprintf(" seq=%d", e.Seq)
	}
	if e.RawTime != 0 {
		s += fmt.Sprintf(" raw_time_ns=%d", e.RawTime)
	}
	if e.Offset != 0 {
		s += fmt.Sprintf(" offset=%d", e.Offset)
	}
	return s
}

// New builds an Error in the given category with a human-readable reason.
func New(cat Category, reason string) *Error {
	return &Error{Category: cat, Reason: reason}
}

// Newf is New with a formatted reason.
func Newf(cat Category, format string, args ...any) *Error {
	return &Error{Category: cat, Reason: fmt.Sprintf(format, args...)}
}

func (e *Error) WithTrack(t string) *Error  { e.Track = t; return e }
func (e *Error) WithOffset(o int64) *Error  { e.Offset = o; return e }
func (e *Error) WithRawTime(ns int64) *Error { e.RawTime = ns; return e }

// WithSeq records the sequence number of the offending unit.
func (e *Error) WithSeq(seq uint32) *Error { e.Seq, e.HasSeq = seq, true; return e }

// List collects classified errors across stages.
type List struct {
	Errors []*Error `json:"errors"`
}

// Add appends an error; nil errors are ignored.
func (l *List) Add(err error) {
	if err == nil {
		return
	}
	if fe, ok := err.(*Error); ok {
		l.Errors = append(l.Errors, fe)
		return
	}
	l.Errors = append(l.Errors, New(CatParse, err.Error()))
}

// Count returns the number of errors in a category.
func (l *List) Count(cat Category) int {
	n := 0
	for _, e := range l.Errors {
		if e.Category == cat {
			n++
		}
	}
	return n
}

// JSON renders the list for durable reporting.
func (l *List) JSON() ([]byte, error) { return json.Marshal(l) }
