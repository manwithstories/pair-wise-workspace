package model

import (
	"errors"
	"fmt"
)

// Process exit codes used by the mediad CLI.
const (
	ExitOK                 = 0  // success
	ExitInputFailure       = 2  // input or verification failure
	ExitInterrupted        = 75 // requested stop, checkpoint safely persisted
	MaxAtomSize      int64 = 512 << 20
)

// OffsetUnknown marks a fault whose exact byte offset cannot be located
// (for example a missing mandatory atom). Valid bad offsets are >= 0.
const OffsetUnknown int64 = -1

// FaultKind enumerates the defect classes the pipeline can detect and
// propagate from indexing through transport and sync.
type FaultKind string

const (
	FaultTruncatedAtom   FaultKind = "truncated_atom"
	FaultOutOfBounds     FaultKind = "out_of_bounds"
	FaultMissingIndex    FaultKind = "missing_index"
	FaultPTSRegression   FaultKind = "pts_regression"
	FaultPTSWrap         FaultKind = "pts_wraparound"
	FaultContainerBounds FaultKind = "container_bounds"
)

// Fault is the shared error model. It carries the exact byte offset of the
// first bad byte inside the source file so every downstream subsystem can
// report and correlate it without re-scanning.
type Fault struct {
	Kind   FaultKind `json:"kind"`
	Offset int64     `json:"offset"`
	Source string    `json:"source,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Error implements error.
func (f *Fault) Error() string {
	if f.Offset == OffsetUnknown {
		return fmt.Sprintf("%s: %s (source=%s)", f.Kind, f.Detail, f.Source)
	}
	return fmt.Sprintf("%s at byte %d: %s (source=%s)", f.Kind, f.Offset, f.Detail, f.Source)
}

// NewFault builds a Fault.
func NewFault(kind FaultKind, offset int64, source, detail string) *Fault {
	return &Fault{Kind: kind, Offset: offset, Source: source, Detail: detail}
}

// AsFault extracts a *Fault from any error in the wrapping chain.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// FirstBadOffset returns the exact offset of the first bad byte, or
// OffsetUnknown when the error carries no byte location.
func FirstBadOffset(err error) int64 {
	if f, ok := AsFault(err); ok {
		return f.Offset
	}
	return OffsetUnknown
}

// Sentinel errors for control flow that has no byte location.
var (
	ErrCheckpointMismatch = errors.New("checkpoint digest mismatch")
	ErrNoCheckpoint       = errors.New("no valid checkpoint found")
	ErrVerification       = errors.New("output verification failed")
	ErrInterrupted        = errors.New("processing interrupted by stop-after limit")
)
