package model

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable error identifier.
type Code string

const (
	CodeMP4BoxLength    Code = "ERR_MP4_BOX_LENGTH"
	CodeKLVLength       Code = "ERR_KLV_LENGTH"
	CodeKLVStructure    Code = "ERR_KLV_STRUCTURE"
	CodeRTPPacket       Code = "ERR_RTP_PACKET"
	CodeTrackDuplicate  Code = "ERR_TRACK_DUPLICATE"
	CodeTrackMissing    Code = "ERR_TRACK_MISSING"
	CodeCropRange       Code = "ERR_CROP_RANGE"
	CodeFingerprint     Code = "ERR_FINGERPRINT_MISMATCH"
	CodeCheckpoint      Code = "ERR_CHECKPOINT"
	CodeTimeline        Code = "ERR_TIMELINE"
	CodeManifestAlign   Code = "ERR_MANIFEST_ALIGN"
	CodeClock           Code = "ERR_CLOCK"
	CodeUsage           Code = "ERR_USAGE"
	CodeInternal        Code = "ERR_INTERNAL"
	CodeDuplicateSample Code = "ERR_DUPLICATE_SAMPLE"
)

// Class decides the process exit class for an error.
type Class uint8

const (
	ClassInput Class = iota + 1
	ClassUsage
)

func (c Code) Class() Class {
	switch c {
	case CodeUsage, CodeInternal:
		return ClassUsage
	default:
		return ClassInput
	}
}

// Error carries a stable code plus the conflicting object (track id and byte
// offset) so operators can locate the exact source of a failure.
type Error struct {
	Code      Code
	TrackID   string
	Offset    int64
	HasOffset bool
	Msg       string
	Cause     error
	Recovered bool
}

// NewError builds a classified error.
func NewError(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// WrapError attaches a stable code to an underlying cause.
func WrapError(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...), Cause: cause}
}

func (e *Error) Error() string {
	s := string(e.Code)
	if e.TrackID != "" {
		s += " track=" + e.TrackID
	}
	if e.HasOffset {
		s += fmt.Sprintf(" offset=%d", e.Offset)
	}
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	if e.Cause != nil {
		s += " (" + e.Cause.Error() + ")"
	}
	return s
}

func (e *Error) Unwrap() error { return e.Cause }

// WithTrack returns a copy tagged with the conflicting track.
func (e *Error) WithTrack(id string) *Error {
	c := *e
	c.TrackID = id
	return &c
}

// WithOffset returns a copy tagged with the source byte offset.
func (e *Error) WithOffset(off int64) *Error {
	c := *e
	c.Offset = off
	c.HasOffset = true
	return &c
}

// Is lets errors.Is match by stable code.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return e.Code == t.Code
}

// ExitCode maps an error to the documented process exit code: 2 for input
// errors (including isolated corruption), 1 for usage/internal errors.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		if e.Code.Class() == ClassUsage {
			return 1
		}
		return 2
	}
	return 1
}

// Tag attaches track context when err is classified, returning it unchanged
// otherwise so plain errors still flow through.
func Tag(err error, trackID string) error {
	var e *Error
	if errors.As(err, &e) {
		return e.WithTrack(trackID)
	}
	return err
}

// TagOffset attaches a byte offset when err is classified.
func TagOffset(err error, off int64) error {
	var e *Error
	if errors.As(err, &e) {
		return e.WithOffset(off)
	}
	return err
}

// CodeOf returns the stable code carried by err, if any.
func CodeOf(err error) (Code, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return "", false
}
