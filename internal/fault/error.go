// Package fault defines the serializable error model shared by every stage of
// the pipeline. Errors carry a Kind, the DAG node that raised them, and the
// list of dependencies blocking progress, so a maintainer can resume from an
// interruption and see exactly why a node is stuck.
package fault

import (
	"encoding/json"
	"fmt"
)

// Kind classifies failures for exit-code and recovery decisions.
type Kind string

const (
	KindCorrupt    Kind = "corrupt_input"   // damaged packets / payload
	KindGap        Kind = "unrecoverable_gap" // late packet beyond reorder window
	KindConflict   Kind = "conflict_packet" // same seq, different payload
	KindCheckpoint Kind = "checkpoint_corrupt"
	KindCycle      Kind = "dependency_cycle"
	KindTimebase   Kind = "timebase_regression"
	KindBlocked    Kind = "blocked_dependency"
	KindRetry      Kind = "retry_exhausted"
)

// Error is a pipeline failure that survives serialization (checkpoints,
// reports) and carries blocking-dependency information.
type Error struct {
	Kind      Kind     `json:"kind"`
	Node      string   `json:"node,omitempty"`
	Msg       string   `json:"msg"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}

func (e *Error) Error() string {
	if e.Node != "" {
		return fmt.Sprintf("%s: node %s: %s", e.Kind, e.Node, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Msg)
}

// New builds an Error for a node (node may be empty for stage-level faults).
func New(kind Kind, node, format string, args ...any) *Error {
	return &Error{Kind: kind, Node: node, Msg: fmt.Sprintf(format, args...)}
}

// WithBlockedBy attaches the dependency list that prevents progress.
func (e *Error) WithBlockedBy(deps ...string) *Error {
	e.BlockedBy = append([]string(nil), deps...)
	return e
}

// Marshal serializes the error for checkpoint storage.
func Marshal(err error) ([]byte, error) {
	if err == nil {
		return []byte("null"), nil
	}
	if fe, ok := err.(*Error); ok {
		return json.Marshal(fe)
	}
	return json.Marshal(&Error{Kind: KindCorrupt, Msg: err.Error()})
}

// Unmarshal restores an Error previously stored with Marshal.
func Unmarshal(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var e Error
	if err := json.Unmarshal(data, &e); err != nil {
		return New(KindCorrupt, "", "undecodable fault record: %v", err)
	}
	return &e
}

// KindOf extracts the Kind of an error, or "" for nil.
func KindOf(err error) Kind {
	if fe, ok := err.(*Error); ok {
		return fe.Kind
	}
	return ""
}
