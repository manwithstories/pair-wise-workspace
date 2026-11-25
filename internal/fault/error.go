// Package fault defines layered, locatable error codes shared by all modules.
package fault

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable failure category.
type Code string

const (
	OK                Code = "OK"
	InvalidArgument   Code = "INVALID_ARGUMENT"
	PathRejected      Code = "PATH_REJECTED"
	TimelineInvalid   Code = "TIMELINE_INVALID"
	IndexInvalid      Code = "INDEX_INVALID"
	AssetDigest       Code = "ASSET_DIGEST_MISMATCH"
	MP4Corrupt        Code = "MP4_CORRUPT"
	MP4Truncated      Code = "MP4_TRUNCATED_SAMPLE"
	PTSRegression     Code = "PTS_REGRESSION"
	RTPInvalid        Code = "RTP_INVALID"
	RenderFailed      Code = "RENDER_FAILED"
	RewrapFailed      Code = "REWRAP_FAILED"
	CheckpointInvalid Code = "CHECKPOINT_INVALID"
	WriteFailed       Code = "OUTPUT_WRITE_FAILED"
	EvidenceMismatch  Code = "EVIDENCE_MISMATCH"
	Interrupted       Code = "SIMULATED_INTERRUPT"
	Internal          Code = "INTERNAL"
)

// Error carries a layer, operation and optional location in addition to a code.
type Error struct {
	Code      Code
	Layer     string
	Op        string
	Path      string
	Segment   int
	HasSeg    bool
	Retryable bool
	Err       error
}

// E constructs a layered fault.
func E(code Code, layer, op string, err error) *Error {
	return &Error{Code: code, Layer: layer, Op: op, Err: err, Segment: -1}
}

// WithPath adds a filesystem location.
func (e *Error) WithPath(path string) *Error { e.Path = path; return e }

// WithSegment adds a segment location.
func (e *Error) WithSegment(segment int) *Error {
	e.Segment = segment
	e.HasSeg = true
	return e
}

// WithRetryable marks whether the operation may succeed on resume.
func (e *Error) WithRetryable(v bool) *Error { e.Retryable = v; return e }

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: layer=%s op=%s", e.Code, e.Layer, e.Op)
	if e.Path != "" {
		msg += " path=" + e.Path
	}
	if e.HasSeg {
		msg += fmt.Sprintf(" segment=%d", e.Segment)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap exposes the causing error.
func (e *Error) Unwrap() error { return e.Err }

// As finds the nearest *Error in err's tree.
func As(err error) (*Error, bool) {
	var fe *Error
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

// IsCode reports whether err contains code c.
func IsCode(err error, c Code) bool {
	fe, ok := As(err)
	return ok && fe.Code == c
}

// ExitCode maps faults to the terminal contract:
// 0 pass, 1 evidence/runtime failure, 2 bad arguments, 75 resumable interrupt.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if IsCode(err, Interrupted) {
		return 75
	}
	if IsCode(err, InvalidArgument) || IsCode(err, PathRejected) {
		return 2
	}
	return 1
}
