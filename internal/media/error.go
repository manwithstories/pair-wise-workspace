// Package media defines the single error model shared by every pipeline stage.
//
// Every failure carries the processing phase, the offending file path, the
// byte offset inside the container (when known) and the media timestamp at
// which it occurred, so that a rejected camera can be traced back to the exact
// source byte range that caused the rejection.
package media

import (
	"errors"
	"fmt"

	"github.com/fashionweek/mediad/internal/clock"
)

// Phase identifies the pipeline stage that produced an error.
type Phase string

const (
	PhaseScan       Phase = "scan"
	PhaseIndex      Phase = "index"
	PhaseAlign      Phase = "align"
	PhaseEncode     Phase = "encode"
	PhasePackage    Phase = "package"
	PhaseCheckpoint Phase = "checkpoint"
	PhaseCache      Phase = "cache"
	PhaseVerify     Phase = "verify"
	PhaseConfig     Phase = "config"
)

// Kind classifies the underlying fault.
type Kind string

const (
	KindTruncated   Kind = "truncated_container"
	KindPTSRollback Kind = "pts_rollback"
	KindBadTimebase Kind = "invalid_timebase"
	KindOverflow    Kind = "overflow"
	KindBounds      Kind = "length_out_of_bounds"
	KindUnsupported Kind = "unsupported_codec"
	KindPath        Kind = "path_rejected"
	KindDigest      Kind = "digest_mismatch"
	KindTool        Kind = "tool_failure"
)

// Error is the structured error type used throughout the pipeline.
type Error struct {
	Phase   Phase
	Kind    Kind
	Path    string
	Offset  int64 // -1 when unknown
	MediaAt clock.Rat
	HasTime bool
	Detail  string
	Cause   error
}

// New builds a structured error; offset -1 means "unknown byte offset".
func New(phase Phase, kind Kind, path string, offset int64, detail string) *Error {
	return &Error{Phase: phase, Kind: kind, Path: path, Offset: offset, Detail: detail}
}

// AtTime attaches the media time at which the fault was observed.
func (e *Error) AtTime(t clock.Rat) *Error {
	e.MediaAt, e.HasTime = t, true
	return e
}

// Wrap attaches a causal error.
func (e *Error) Wrap(cause error) *Error {
	e.Cause = cause
	return e
}

// Error implements error.
func (e *Error) Error() string {
	s := fmt.Sprintf("%s/%s: %s", e.Phase, e.Kind, e.Detail)
	if e.Path != "" {
		s += " path=" + e.Path
	}
	if e.Offset >= 0 {
		s += fmt.Sprintf(" byte_offset=%d", e.Offset)
	}
	if e.HasTime {
		s += " media_time=" + e.MediaAt.String()
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// Unwrap exposes the causal chain for errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }

// As extracts a *media.Error from any wrapped error chain.
func As(err error) (*Error, bool) {
	var me *Error
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// IsKind reports whether err (or any error in its chain) carries kind.
func IsKind(err error, kind Kind) bool {
	me, ok := As(err)
	return ok && me.Kind == kind
}
