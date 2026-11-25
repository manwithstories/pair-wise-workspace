package media

import "fmt"

// RecoveryAction tells an operator how the pipeline handled (or should
// handle) an input defect.
type RecoveryAction uint8

const (
	// ActionFail: processing cannot continue; input is unrecoverable.
	ActionFail RecoveryAction = iota
	// ActionTruncate: input was cut at the last complete record.
	ActionTruncate
	// ActionGap: the defective unit was recorded as a timeline gap.
	ActionGap
	// ActionFallbackIndex: the index was missing/unusable; PTS were derived.
	ActionFallbackIndex
	// ActionRecovered: parity/FEC reconstruction succeeded.
	ActionRecovered
	// ActionLimit: a bounded queue/cache limit was reached.
	ActionLimit
	// ActionCancel: processing was cancelled before completion.
	ActionCancel
)

// String renders the action for terminal/metric output.
func (a RecoveryAction) String() string {
	switch a {
	case ActionFail:
		return "fail"
	case ActionTruncate:
		return "truncate"
	case ActionGap:
		return "gap"
	case ActionFallbackIndex:
		return "index-fallback"
	case ActionRecovered:
		return "recovered"
	case ActionLimit:
		return "limit"
	case ActionCancel:
		return "cancel"
	default:
		return "unknown"
	}
}

// Error is the locatable error model shared by all subsystems. It carries the
// byte offset (or sequence number) where the defect was observed and the
// recovery action taken, so operators can locate and repair bad records.
type Error struct {
	// Subsystem tags the producing component, e.g. "parser", "rtp".
	Subsystem string
	// Kind is a short stable identifier, e.g. "checksum_mismatch".
	Kind string
	// Offset is the byte offset in the input stream when applicable.
	Offset int64
	// Sequence is the record/packet sequence when applicable.
	Sequence uint32
	// HasSequence reports whether Sequence is meaningful.
	HasSequence bool
	// Action is the recovery action applied.
	Action RecoveryAction
	// Message is a human-readable description.
	Message string
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.HasSequence {
		return fmt.Sprintf("%s: %s at seq=%d offset=%d action=%s: %s",
			e.Subsystem, e.Kind, e.Sequence, e.Offset, e.Action, e.Message)
	}
	return fmt.Sprintf("%s: %s at offset=%d action=%s: %s",
		e.Subsystem, e.Kind, e.Offset, e.Action, e.Message)
}

// NewError builds a locatable error with a byte offset.
func NewError(subsystem, kind string, offset int64, action RecoveryAction, format string, args ...any) *Error {
	return &Error{
		Subsystem: subsystem,
		Kind:      kind,
		Offset:    offset,
		Action:    action,
		Message:   fmt.Sprintf(format, args...),
	}
}

// NewSeqError builds a locatable error keyed on a sequence number.
func NewSeqError(subsystem, kind string, seq uint32, action RecoveryAction, format string, args ...any) *Error {
	return &Error{
		Subsystem:   subsystem,
		Kind:        kind,
		Sequence:    seq,
		HasSequence: true,
		Action:      action,
		Message:     fmt.Sprintf(format, args...),
	}
}

// Sentinel error kinds for matching with errors.Is.
const (
	KindUnknownVersion = "unknown_version"
	KindTruncated      = "truncated_record"
	KindChecksum       = "checksum_mismatch"
	KindPTSRegression  = "pts_regression"
	KindMissingIndex   = "missing_index"
	KindOverflow       = "integer_overflow"
	KindOutOfBounds    = "out_of_bounds"
	KindLimitExceeded  = "limit_exceeded"
	KindCancelled      = "cancelled"
	KindUnrecoverable  = "unrecoverable"
)
