package clock

import (
	"errors"
	"fmt"
)

// ErrorCode is the stable, machine-readable classification carried by every
// structured error so stages downstream can react without parsing messages.
type ErrorCode string

const (
	CodeIllegalFrame   ErrorCode = "ILLEGAL_FRAME"
	CodeRollback       ErrorCode = "ROLLBACK"
	CodeDuplicateTS    ErrorCode = "DUPLICATE_TIMESTAMP"
	CodeRepairTooLarge ErrorCode = "REPAIR_TOO_LARGE"
	CodeSubtitleOverlap ErrorCode = "SUBTITLE_OVERLAP"
	CodeSubtitleParse  ErrorCode = "SUBTITLE_PARSE"
	CodeGraphCycle     ErrorCode = "GRAPH_CYCLE"
	CodeUnknownLayout  ErrorCode = "UNKNOWN_LAYOUT"
	CodeSampleRate     ErrorCode = "SAMPLE_RATE_CONFLICT"
	CodeColorConflict  ErrorCode = "COLOR_METADATA_CONFLICT"
	CodeTruncated      ErrorCode = "INPUT_TRUNCATED"
	CodeInterrupted    ErrorCode = "INPUT_INTERRUPTED"
	CodeChecksum       ErrorCode = "CHECKSUM_MISMATCH"
	CodeNonMonotonic   ErrorCode = "NON_MONOTONIC_INDEX"
	CodeBadOffset      ErrorCode = "BAD_OFFSET"
)

// Interval is the half-open [Start, End) nanosecond span an error applies to.
type Interval struct {
	Start int64
	End   int64
}

// RepairAction describes how the pipeline handled a defect.
type RepairAction string

const (
	ActionNone      RepairAction = "none"
	ActionNudged    RepairAction = "nudged"
	ActionShifted   RepairAction = "shifted"
	ActionTrimmed   RepairAction = "trimmed"
	ActionIsolated  RepairAction = "isolated"
	ActionResampled RepairAction = "resampled"
	ActionMapped    RepairAction = "channel_mapped"
	ActionRecovered RepairAction = "recovered"
)

// TrackError is the common error model across clock, subtitle, track and index
// modules: every defect carries its track, interval, code and the action taken.
type TrackError struct {
	Code    ErrorCode    `json:"code"`
	Track   string       `json:"track"`
	Where   Interval     `json:"where"`
	Action  RepairAction `json:"action"`
	Message string       `json:"message"`
}

// NewError builds a structured error with Action defaulting to none.
func NewError(code ErrorCode, track string, where Interval, msg string) *TrackError {
	return &TrackError{Code: code, Track: track, Where: where, Action: ActionNone, Message: msg}
}

// Error implements the error interface.
func (e *TrackError) Error() string {
	loc := ""
	if e.Where.End > e.Where.Start {
		loc = fmt.Sprintf(" [%d..%d]", e.Where.Start, e.Where.End)
	} else if e.Where.Start != 0 || e.Where.End != 0 {
		loc = fmt.Sprintf(" @%d", e.Where.Start)
	}
	t := e.Track
	if t == "" {
		t = "-"
	}
	return fmt.Sprintf("%s/%s%s: %s", t, e.Code, loc, e.Message)
}

// WithAction returns a copy carrying the given repair action.
func (e *TrackError) WithAction(a RepairAction) *TrackError {
	cp := *e
	cp.Action = a
	return &cp
}

// Is supports errors.Is matching on the stable code.
func (e *TrackError) Is(target error) bool {
	var te *TrackError
	if errors.As(target, &te) {
		return te.Code == e.Code
	}
	return false
}

// HasCode reports whether err (or any wrapped error) carries the given code.
func HasCode(err error, code ErrorCode) bool {
	var te *TrackError
	if errors.As(err, &te) {
		return te.Code == code
	}
	return false
}
