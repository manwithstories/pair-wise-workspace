// Package fault provides the shared error model used by every stage of the
// medialine pipeline. Every failure names the stage, the track, and the byte
// offset (or timestamp offset) at which it was observed so that an operator
// can identify the responsible fragment after a batch run.
package fault

import (
	"errors"
	"fmt"
)

// Stage identifies the subsystem that produced an error.
type Stage string

const (
	StageParse    Stage = "parse"
	StageClock    Stage = "clock"
	StageIndex    Stage = "index"
	StageAlign    Stage = "align"
	StageWrite    Stage = "write"
	StageVerify   Stage = "verify"
	StageCancel   Stage = "cancel"
	StageConfig   Stage = "config"
	UnknownTrack        = -1
	UnknownOffset       = -1
)

// Kind is a stable machine-readable classification of a fault.
type Kind string

const (
	KindTruncatedBox  Kind = "truncated_box"
	KindInvalidLength Kind = "invalid_length"
	KindCorruptSample Kind = "corrupt_sample"
	KindMissingIndex  Kind = "missing_index"
	KindAmbiguousTime Kind = "ambiguous_time"
	KindDuplicateTime Kind = "duplicate_time"
	KindBackwardsTime Kind = "backwards_time"
	KindZeroDuration  Kind = "zero_duration"
	KindPathEscape    Kind = "path_escape"
	KindWrite         Kind = "write_failure"
	KindCanceled      Kind = "canceled"
	KindBadConfig     Kind = "bad_config"
)

// Error is the shared fault record. Track and Offset are populated as the
// error bubbles up through parse, align and write stages.
type Error struct {
	Stage   Stage
	Kind    Kind
	TrackID int
	Offset  int64
	Segment string
	Message string
	Err     error
}

// New constructs a fault with unknown track and offset.
func New(stage Stage, kind Kind, format string, args ...interface{}) *Error {
	return &Error{Stage: stage, Kind: kind, TrackID: UnknownTrack, Offset: UnknownOffset, Message: fmt.Sprintf(format, args...)}
}

// At adds track/offset context. It never mutates a shared error: a copy is
// returned so concurrent workers can annotate the same root fault safely.
func (e *Error) At(trackID int, offset int64) *Error {
	if e == nil {
		return nil
	}
	c := *e
	if trackID != UnknownTrack {
		c.TrackID = trackID
	}
	if offset != UnknownOffset {
		c.Offset = offset
	}
	return &c
}

// WithSegment names the fragment responsible for the fault.
func (e *Error) WithSegment(id string) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Segment = id
	return &c
}

func (e *Error) Error() string {
	track, offset := "?", "?"
	if e.TrackID != UnknownTrack {
		track = fmt.Sprint(e.TrackID)
	}
	if e.Offset != UnknownOffset {
		offset = fmt.Sprint(e.Offset)
	}
	loc := fmt.Sprintf("stage=%s kind=%s track=%s offset=%s", e.Stage, e.Kind, track, offset)
	if e.Segment != "" {
		loc += " segment=" + e.Segment
	}
	if e.Message != "" {
		loc += ": " + e.Message
	}
	if e.Err != nil {
		loc += ": " + e.Err.Error()
	}
	return loc
}

func (e *Error) Unwrap() error { return e.Err }

// From converts err into a *fault.Error, taking an existing fault as-is and
// wrapping any other error at the supplied stage/kind.
func From(err error, stage Stage, kind Kind) *Error {
	if err == nil {
		return nil
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe
	}
	return &Error{Stage: stage, Kind: kind, TrackID: UnknownTrack, Offset: UnknownOffset, Err: err}
}

// As extracts the first fault in the chain.
func As(err error) (*Error, bool) {
	var fe *Error
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

// IsCanceled reports whether err carries the cancellation classification.
func IsCanceled(err error) bool {
	fe, ok := As(err)
	return ok && fe.Kind == KindCanceled
}

// IsRecoverable reports whether a fault permits the pipeline to keep working
// on other fragments: parse, index and sample faults are isolated to one
// fragment, while config/path and write faults are not.
func IsRecoverable(err error) bool {
	fe, ok := As(err)
	if !ok {
		return false
	}
	switch fe.Kind {
	case KindTruncatedBox, KindInvalidLength, KindCorruptSample, KindMissingIndex,
		KindAmbiguousTime, KindDuplicateTime, KindBackwardsTime, KindZeroDuration:
		return true
	default:
		return false
	}
}
