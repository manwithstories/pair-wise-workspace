// Package model defines the serializable error model shared by every
// subsystem. Errors crossing module boundaries are always *MediaError
// so damage reports, boundary failures and checkpoint mismatches keep
// their diagnostic context (offset, media time, representation,
// segment index) all the way to the terminal.
package model

import "encoding/json"

// Stage identifies the subsystem that raised an error.
type Stage string

const (
	StageProbe     Stage = "probe"
	StageNormalize Stage = "normalize"
	StageSegment   Stage = "segment"
	StageFanout    Stage = "fanout"
	StageRecovery  Stage = "recovery"
	StageVerify    Stage = "verify"
)

// Process exit codes required by the acceptance flow.
const (
	ExitOK                 = 0
	ExitDamagedInput       = 2 // input media is damaged
	ExitBoundaryMismatch   = 3 // fan-in boundary check failed
	ExitCheckpointMismatch = 4 // checkpoint refused to resume
)

// MediaError is the canonical, JSON-serializable error.
type MediaError struct {
	Stage          Stage  `json:"stage"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	Representation string `json:"representation,omitempty"`
	SegmentIndex   int    `json:"segment_index"`
	// Damage localization (probe / verify).
	OffsetBytes int64 `json:"damaged_offset_bytes,omitempty"`
	MediaTimeMs int64 `json:"damaged_time_ms,omitempty"`
	// Boundary divergence (fan-in).
	DeltaMs int64 `json:"delta_ms,omitempty"`
	// Exit is the process exit code this error maps to.
	Exit int `json:"-"`
}

// New builds a MediaError for a stage/code with the mapped exit code.
func New(stage Stage, code, msg string, exit int) *MediaError {
	return &MediaError{Stage: stage, Code: code, Message: msg, SegmentIndex: -1, Exit: exit}
}

func (e *MediaError) Error() string {
	b, _ := json.Marshal(e)
	return string(b)
}

// ExitCode maps any error to a process exit code.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	if me, ok := err.(*MediaError); ok && me.Exit != 0 {
		return me.Exit
	}
	return 1
}
