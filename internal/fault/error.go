// Package fault defines the single error model shared by all mediapack
// stages.  Every detected problem is attributed to a track and, where
// applicable, a frame or segment, and every propagated failure records the
// first dependency that blocked it.  Nothing in the pipeline is allowed to
// wrap a stage failure in a bare fmt.Errorf: doing so would lose the
// attribution that operators rely on.
package fault

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Class classifies a failure.  Classes map directly to process exit codes:
// ClassInput and ClassDependency exit with code 2, ClassUsage with code 1.
type Class string

const (
	// ClassInput marks malformed or corrupt media input.
	ClassInput Class = "input"
	// ClassUsage marks invalid command line or configuration.
	ClassUsage Class = "usage"
	// ClassDependency marks a node blocked by an earlier node.
	ClassDependency Class = "dependency"
	// ClassLimit marks an input that exceeds a configured safety limit.
	ClassLimit Class = "limit"
	// ClassInternal marks an unexpected programming error.
	ClassInternal Class = "internal"
)

// Error is a first-class, attributable failure.
type Error struct {
	// Class selects the exit-code bucket.
	Class Class
	// Track is the logical input track; "*" means not track specific.
	Track string
	// Frame is the public frame number (0 when not frame specific).
	Frame int
	// Segment is the segment ordinal (0 when not segment specific).
	Segment int
	// Stage is the pipeline stage that detected the problem.
	Stage string
	// Code is a short stable machine-readable reason.
	Code string
	// Msg is human readable detail.
	Msg string
	// BlockedBy is the first dependency that prevented this unit of work.
	BlockedBy string
	// Cause preserves the underlying error, if any.
	Cause error
}

// New builds an attributable error.
func New(class Class, stage, track, code, msg string) *Error {
	return &Error{Class: class, Stage: stage, Track: track, Code: code, Msg: msg}
}

// Wrap builds an attributable error around cause.
func Wrap(class Class, stage, track, code, msg string, cause error) *Error {
	return &Error{Class: class, Stage: stage, Track: track, Code: code, Msg: msg, Cause: cause}
}

// WithFrame attaches a frame number.
func (e *Error) WithFrame(frame int) *Error {
	e.Frame = frame
	return e
}

// WithSegment attaches a segment ordinal.
func (e *Error) WithSegment(segment int) *Error {
	e.Segment = segment
	return e
}

// WithBlockedBy records the first blocking dependency.
func (e *Error) WithBlockedBy(dep string) *Error {
	e.BlockedBy = dep
	return e
}

// Error implements error.  It includes the exact attribution fields so the
// terminal report can be parsed without inspecting the struct.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Class))
	b.WriteString(" error")
	if e.Stage != "" {
		b.WriteString(" stage=")
		b.WriteString(e.Stage)
	}
	if e.Track != "" {
		b.WriteString(" track=")
		b.WriteString(e.Track)
	}
	if e.Frame > 0 {
		fmt.Fprintf(&b, " frame=%d", e.Frame)
	}
	if e.Segment > 0 {
		fmt.Fprintf(&b, " segment=%d", e.Segment)
	}
	if e.Code != "" {
		b.WriteString(" code=")
		b.WriteString(e.Code)
	}
	if e.Msg != "" {
		b.WriteString(": ")
		b.WriteString(e.Msg)
	}
	if e.BlockedBy != "" {
		b.WriteString(" blocked_by=")
		b.WriteString(e.BlockedBy)
	}
	if e.Cause != nil {
		b.WriteString(" cause=")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap exposes the cause chain.
func (e *Error) Unwrap() error { return e.Cause }

// As extracts the first *Error from err's chain.
func As(err error) (*Error, bool) {
	var fe *Error
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

// ClassOf returns the class of err, defaulting to ClassInternal for plain
// errors.  This keeps exit-code selection total.
func ClassOf(err error) Class {
	if err == nil {
		return ""
	}
	if fe, ok := As(err); ok {
		return fe.Class
	}
	return ClassInternal
}

// ExitCode maps an error to the documented process exit code: 2 for any
// detected input, limit or dependency problem, 1 for usage, 0 for nil.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch ClassOf(err) {
	case ClassUsage:
		return 1
	case ClassInternal:
		return 1
	default:
		return 2
	}
}

// FirstBlocked returns the earliest (lowest frame, then segment) blocked
// error across errs.  It is used when a stage has several failed units and
// the terminal must report the single earliest blocking dependency.
func FirstBlocked(errs []error) *Error {
	candidates := make([]*Error, 0, len(errs))
	for _, err := range errs {
		if fe, ok := As(err); ok && fe.Class == ClassDependency {
			candidates = append(candidates, fe)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Unnumbered findings (Frame == 0) come after every numbered one: a
	// framework-level block is reported only when no unit-level block exists.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (a.Frame == 0) != (b.Frame == 0) {
			return a.Frame != 0
		}
		if a.Frame != b.Frame {
			return a.Frame < b.Frame
		}
		if a.Segment != b.Segment {
			return a.Segment < b.Segment
		}
		return a.Track < b.Track
	})
	return candidates[0]
}

// Blocked builds a dependency error that names the first blocking unit.
func Blocked(stage, track string, blockedBy string, frame, segment int) *Error {
	return &Error{
		Class:     ClassDependency,
		Stage:     stage,
		Track:     track,
		Frame:     frame,
		Segment:   segment,
		Code:      "blocked",
		Msg:       "blocked by earlier failure",
		BlockedBy: blockedBy,
	}
}
