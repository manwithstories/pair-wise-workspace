package media

import (
	"fmt"
)

// Class classifies pipeline failures so the DAG runner can choose between
// rejecting an input (exit code 2) and retrying/recovering (exit code 3).
type Class int

const (
	// ClassBoundary: input violates a declared or structural boundary
	// (truncation, out-of-range block, illegal mapping). Not retryable.
	ClassBoundary Class = iota + 1
	// ClassConflict: metadata or streams conflict and no rule resolves
	// them (channel layout, sample rate, colour metadata). Not retryable;
	// the first blocking dependency is reported.
	ClassConflict
	// ClassInternal: a local processing or atomic-commit failure that a
	// rerun may recover from.
	ClassInternal
)

func (c Class) String() string {
	switch c {
	case ClassBoundary:
		return "boundary"
	case ClassConflict:
		return "conflict"
	case ClassInternal:
		return "internal"
	default:
		return fmt.Sprintf("class-%d", int(c))
	}
}

// PipelineError carries enough provenance to trace a failure from its
// origin to every blocked downstream node: the failing node, the affected
// track and block offset (zero means "not applicable"), and the node whose
// absence/error first blocks the current operation.
type PipelineError struct {
	Node       string
	Class      Class
	Track      uint32
	BlockOff   int64 // absolute byte offset of the offending block, -1 if none
	Dependency string
	Reason     string
	Cause      error
}

// Error implements error.
func (e *PipelineError) Error() string {
	s := fmt.Sprintf("media: node=%q class=%s", e.Node, e.Class)
	if e.Track != 0 {
		s += fmt.Sprintf(" track=%d", e.Track)
	}
	if e.BlockOff >= 0 {
		s += fmt.Sprintf(" blockOffset=%d", e.BlockOff)
	}
	if e.Dependency != "" {
		s += fmt.Sprintf(" blockedBy=%q", e.Dependency)
	}
	s += ": " + e.Reason
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// Unwrap exposes the underlying cause for errors.Is/As.
func (e *PipelineError) Unwrap() error { return e.Cause }

// NewBoundary builds a structural/input-boundary error.
func NewBoundary(node, reason string, track uint32, blockOff int64, cause error) *PipelineError {
	return &PipelineError{Node: node, Class: ClassBoundary, Track: track,
		BlockOff: blockOff, Reason: reason, Cause: cause}
}

// NewConflict builds a metadata/stream conflict naming the first blocking
// dependency; empty dependency means the conflict is intrinsic to the node.
func NewConflict(node, reason, dependency string) *PipelineError {
	return &PipelineError{Node: node, Class: ClassConflict, BlockOff: -1,
		Dependency: dependency, Reason: reason}
}

// NewInternal builds a retryable processing/IO error.
func NewInternal(node, reason string, cause error) *PipelineError {
	return &PipelineError{Node: node, Class: ClassInternal, BlockOff: -1,
		Reason: reason, Cause: cause}
}

// BlockedBy wraps an upstream failure for a downstream node, recording the
// dependency chain while preserving the original class.
func BlockedBy(node string, upstream *PipelineError) *PipelineError {
	if upstream == nil {
		return NewInternal(node, "blocked by nil upstream", nil)
	}
	dep := upstream.Node
	return &PipelineError{
		Node: node, Class: upstream.Class, Track: upstream.Track,
		BlockOff: upstream.BlockOff, Dependency: dep,
		Reason:  "upstream failure blocks this node",
		Cause:   upstream,
	}
}

// FirstBlocker walks the dependency chain and returns the originating
// blocker name: the root error's explicit Dependency when present,
// otherwise the root node name. Returns "" for a non-pipeline error.
func FirstBlocker(err error) string {
	pe, ok := err.(*PipelineError)
	if !ok {
		return ""
	}
	root := pe
	for {
		inner, ok := root.Cause.(*PipelineError)
		if !ok {
			break
		}
		root = inner
	}
	if root.Dependency != "" {
		return root.Dependency
	}
	return root.Node
}
