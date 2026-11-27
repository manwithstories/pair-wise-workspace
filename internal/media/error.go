package media

import (
	"errors"
	"fmt"
)

// ExitCode is the process status contract for the demo and all subcommands.
type ExitCode int

const (
	// ExitOK means the demo and every verification passed.
	ExitOK ExitCode = 0
	// ExitUsage means a parameter or dependency graph was rejected.
	ExitUsage ExitCode = 2
	// ExitInput means strict-mode input corruption was detected.
	ExitInput ExitCode = 3
	// ExitInternal means I/O, cancellation or an internal processing fault.
	ExitInternal ExitCode = 4
)

// Phase names the pipeline stage that produced an error.
type Phase string

const (
	PhaseConfig   Phase = "config"
	PhaseInput    Phase = "input"
	PhaseTimeline Phase = "timeline"
	PhaseGraph    Phase = "graph"
	PhaseQueue    Phase = "queue"
	PhaseResample Phase = "resample"
	PhaseLayout   Phase = "layout"
	PhaseLoudness Phase = "loudness"
	PhaseAudit    Phase = "audit"
	PhaseIO       Phase = "io"
)

// Error is the single error type shared by wav, resample, layout, loudness and
// the scheduler. Every failure carries the stage, the track, the block number
// and whether a recovery pass may retry the work.
type Error struct {
	Phase       Phase
	Exit        ExitCode
	Track       string
	Block       uint64
	Recoverable bool
	Op          string
	Err         error
}

// NewError wraps err with pipeline context. A nil err yields nil so callers can
// return NewError(...) directly.
func NewError(phase Phase, exit ExitCode, track string, block uint64, recoverable bool, op string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{
		Phase:       phase,
		Exit:        exit,
		Track:       track,
		Block:       block,
		Recoverable: recoverable,
		Op:          op,
		Err:         err,
	}
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	loc := string(e.Phase)
	if e.Track != "" {
		loc += fmt.Sprintf(" track=%s", e.Track)
	}
	if e.Op != "" {
		loc += fmt.Sprintf(" op=%s", e.Op)
	}
	loc += fmt.Sprintf(" block=%d", e.Block)
	if e.Recoverable {
		loc += " recoverable=true"
	}
	if e.Err == nil {
		return loc
	}
	return loc + ": " + e.Err.Error()
}

// Unwrap exposes the underlying cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsRecoverable reports whether the failure is eligible for a recovery pass.
func (e *Error) IsRecoverable() bool {
	return e != nil && e.Recoverable
}

// AsError extracts the pipeline error from an arbitrary error chain.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// ExitFor maps an error to the process exit code. Unknown errors are internal.
func ExitFor(err error) ExitCode {
	if err == nil {
		return ExitOK
	}
	if pe, ok := AsError(err); ok {
		return pe.Exit
	}
	return ExitInternal
}

// Sentinel causes. Wrapping them keeps errors.Is portable for callers that do
// not need the full pipeline context.
var (
	ErrCancelled    = errors.New("processing cancelled")
	ErrCorruptInput = errors.New("corrupt input")
	ErrLimit        = errors.New("limit exceeded")
	ErrConflict     = errors.New("conflicting duplicate block")
)
