package model

import (
	"errors"
	"fmt"
)

type Stage string

const (
	StageCLI        Stage = "cli"
	StageTransport  Stage = "transport"
	StageScan       Stage = "scan"
	StageTables     Stage = "tables"
	StageMap        Stage = "map"
	StagePipeline   Stage = "pipeline"
	StageIndex      Stage = "index"
	StageCheckpoint Stage = "checkpoint"
	StageFixture    Stage = "fixture"
)

type Code string

const (
	CodeInvalidArgument   Code = "invalid_argument"
	CodePathEscape        Code = "path_escape"
	CodePacketTooLarge    Code = "packet_too_large"
	CodeBufferLimit       Code = "buffer_limit"
	CodeDuplicateConflict Code = "duplicate_conflict"
	CodeRetryExhausted    Code = "retransmit_exhausted"
	CodeIncomplete        Code = "incomplete_input"
	CodeCorruptBox        Code = "corrupt_box"
	CodeUnsupported       Code = "unsupported_format"
	CodeEncryptedTrack    Code = "encrypted_track"
	CodeAmbiguousMap      Code = "ambiguous_map"
	CodeTimestampBackward Code = "timestamp_backward"
	CodeIndexStale        Code = "index_stale"
	CodeIndexTruncated    Code = "index_truncated"
	CodeVersionMismatch   Code = "version_mismatch"
	CodeCheckpointCorrupt Code = "checkpoint_corrupt"
	CodeIntegrity         Code = "integrity_mismatch"
	CodeInternal          Code = "internal"
)

// Error is the common cross-stage error envelope. Location fields are optional
// and are preserved while an error is wrapped upward.
type Error struct {
	Code           Code   `json:"code"`
	Stage          Stage  `json:"stage"`
	Message        string `json:"message"`
	Source         string `json:"source,omitempty"`
	TrackID        uint32 `json:"track_id,omitempty"`
	HasTrack       bool   `json:"has_track,omitempty"`
	BoxType        string `json:"box_type,omitempty"`
	Offset         int64  `json:"offset,omitempty"`
	HasOffset      bool   `json:"has_offset,omitempty"`
	ExpectedOffset int64  `json:"expected_offset,omitempty"`
	DetectedOffset int64  `json:"detected_offset,omitempty"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	ActualSHA256   string `json:"actual_sha256,omitempty"`
	cause          error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s/%s: %s", e.Stage, e.Code, e.Message)
	if e.Source != "" {
		s += " source=" + e.Source
	}
	if e.HasTrack {
		s += fmt.Sprintf(" track=%d", e.TrackID)
	}
	if e.BoxType != "" {
		s += " box=" + e.BoxType
	}
	if e.HasOffset {
		s += fmt.Sprintf(" offset=%d", e.Offset)
	}
	if e.cause != nil {
		s += ": " + e.cause.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.cause }

func New(code Code, stage Stage, message string) *Error {
	return &Error{Code: code, Stage: stage, Message: message}
}

func Newf(code Code, stage Stage, format string, args ...any) *Error {
	return New(code, stage, fmt.Sprintf(format, args...))
}

func Wrap(code Code, stage Stage, message string, cause error) *Error {
	if cause == nil {
		return New(code, stage, message)
	}
	// Preserve an existing structured location while changing stage/code.
	var src *Error
	if errors.As(cause, &src) {
		cp := *src
		cp.Code = code
		cp.Stage = stage
		cp.Message = message
		cp.cause = cause
		return &cp
	}
	return &Error{Code: code, Stage: stage, Message: message, cause: cause}
}

func (e *Error) WithSource(source string) *Error { e.Source = source; return e }
func (e *Error) WithTrack(id uint32) *Error      { e.TrackID, e.HasTrack = id, true; return e }
func (e *Error) WithBox(t string) *Error         { e.BoxType = t; return e }
func (e *Error) WithOffset(off int64) *Error     { e.Offset, e.HasOffset = off, true; return e }
func (e *Error) WithExpectedOffset(off int64) *Error {
	e.ExpectedOffset, e.DetectedOffset = off, off
	e.Offset, e.HasOffset = off, true
	return e
}
func (e *Error) WithDetectedOffset(expected, detected int64) *Error {
	e.ExpectedOffset = expected
	e.DetectedOffset = detected
	e.Offset, e.HasOffset = detected, true
	return e
}
func (e *Error) WithDigest(expected, actual string) *Error {
	e.ExpectedSHA256, e.ActualSHA256 = expected, actual
	return e
}

// AsError finds the shared structured error in a wrap chain.
func AsError(err error) (*Error, bool) {
	var me *Error
	return me, errors.As(err, &me)
}

// ExitCode maps failures to the CLI contract in the task specification.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var me *Error
	if !errors.As(err, &me) {
		return 1
	}
	switch me.Code {
	case CodeInvalidArgument, CodePathEscape:
		return 2
	case CodeCorruptBox:
		return 3
	case CodeRetryExhausted, CodeIncomplete, CodeIntegrity, CodeCheckpointCorrupt:
		return 4
	default:
		return 1
	}
}
