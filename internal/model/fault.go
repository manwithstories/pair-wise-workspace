package model

import (
	"errors"
	"fmt"
)

// Phase identifies the pipeline stage that produced an Error.
type Phase string

const (
	PhaseEBML        Phase = "ebml"
	PhaseDemux       Phase = "demux"
	PhaseSegment     Phase = "segment"
	PhaseAudio       Phase = "audio"
	PhaseFingerprint Phase = "fingerprint"
	PhasePipeline    Phase = "pipeline"
	PhaseVerify      Phase = "verify"
	PhaseRTP         Phase = "rtp"
	PhaseMP4         Phase = "mp4"
)

// Error carries the original source offset and recovery classification.
type Error struct {
	Phase          Phase
	Offset         int64
	EndOffset      int64
	RecoveryOffset int64
	Recoverable    bool
	// Invariant maps to process exit code 2 instead of invalid-input code 1.
	Invariant bool
	Reason    string
	Err       error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s error at offset %d: %s", e.Phase, e.Offset, e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if e.RecoveryOffset > e.Offset {
		msg += fmt.Sprintf(" (recovery_offset=%d)", e.RecoveryOffset)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// NewError constructs a structured media error.
func NewError(phase Phase, offset int64, recoverable bool, reason string, err error) *Error {
	return &Error{Phase: phase, Offset: offset, Recoverable: recoverable, Reason: reason, Err: err}
}

// InvariantError constructs a verification invariant failure.
func InvariantError(phase Phase, offset int64, reason string, err error) *Error {
	return &Error{
		Phase:       phase,
		Offset:      offset,
		Recoverable: false,
		Invariant:   true,
		Reason:      reason,
		Err:         err,
	}
}

// AsError finds a structured Error in err's wrap chain.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// ExitCode maps processing errors to the command's stable exit codes.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := AsError(err); ok && e.Invariant {
		return 2
	}
	return 1
}
