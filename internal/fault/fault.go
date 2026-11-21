// Package fault defines the unified, locatable, recoverable error model
// shared by the timeline, stream, captions and pipeline subsystems.
package fault

import "fmt"

// Category identifies the subsystem that raised an error.
type Category string

const (
	CatTimecode   Category = "timecode"
	CatCaption    Category = "caption"
	CatStream     Category = "stream"
	CatRTP        Category = "rtp"
	CatPipeline   Category = "pipeline"
	CatCheckpoint Category = "checkpoint"
)

// Error is a locatable error. Offset carries a file byte offset for
// file-based parsers, or a packet sequence number for RTP streams;
// it is -1 when no location applies. Recoverable errors are collected
// and reported, never silently dropped; fatal errors abort the stage.
type Error struct {
	Cat         Category `json:"category"`
	Msg         string   `json:"message"`
	Offset      int64    `json:"offset"`
	Recoverable bool     `json:"recoverable"`
}

func (e *Error) Error() string {
	loc := ""
	if e.Offset >= 0 {
		loc = fmt.Sprintf(" @%d", e.Offset)
	}
	kind := "fatal"
	if e.Recoverable {
		kind = "recoverable"
	}
	return fmt.Sprintf("[%s]%s %s: %s", e.Cat, loc, kind, e.Msg)
}

// New builds an Error. offset < 0 means "no location".
func New(cat Category, offset int64, recoverable bool, format string, args ...any) *Error {
	return &Error{Cat: cat, Msg: fmt.Sprintf(format, args...), Offset: offset, Recoverable: recoverable}
}

// List collects errors; recoverable ones accumulate while fatal ones
// mark the list failed but do not stop further collection.
type List struct {
	Errs []*Error `json:"errors,omitempty"`
}

func (l *List) Add(err error) {
	if err == nil {
		return
	}
	if fe, ok := err.(*Error); ok {
		l.Errs = append(l.Errs, fe)
		return
	}
	l.Errs = append(l.Errs, &Error{Cat: CatPipeline, Msg: err.Error(), Offset: -1})
}

// Addf appends a new categorized error.
func (l *List) Addf(cat Category, offset int64, recoverable bool, format string, args ...any) {
	l.Errs = append(l.Errs, New(cat, offset, recoverable, format, args...))
}

// Fatal reports whether any collected error is non-recoverable.
func (l *List) Fatal() bool {
	for _, e := range l.Errs {
		if !e.Recoverable {
			return true
		}
	}
	return false
}

// Len returns the number of collected errors.
func (l *List) Len() int { return len(l.Errs) }

// ByCategory counts errors per category, for report summaries.
func (l *List) ByCategory() map[Category]int {
	m := map[Category]int{}
	for _, e := range l.Errs {
		m[e.Cat]++
	}
	return m
}
