// Package clock defines the shared 90 kHz media clock, timelines and the
// recoverable error model used by the RTP, audio and transaction modules.
package clock

import "fmt"

// Stage identifies the pipeline stage that produced an error.
type Stage string

const (
	StageIngest   Stage = "ingest"
	StageRTCP     Stage = "rtcp"
	StageBuffer   Stage = "buffer"
	StageConvert  Stage = "convert"
	StagePLC      Stage = "plc"
	StageLoudness Stage = "loudness"
	StageWAL      Stage = "wal"
	StageCommit   Stage = "commit"
	StageMetrics  Stage = "metrics"
)

// ErrCode is a stable, locatable error code. Timeouts always carry
// ErrTimeout so callers can distinguish them from plain failures.
type ErrCode string

const (
	ErrInvalidPacket   ErrCode = "invalid_packet"
	ErrInvalidParam    ErrCode = "invalid_param"
	ErrInvalidPath     ErrCode = "invalid_path"
	ErrGap             ErrCode = "sequence_gap"
	ErrDuplicate       ErrCode = "duplicate_packet"
	ErrWrap            ErrCode = "counter_wrap"
	ErrNoReport        ErrCode = "no_rtcp_report"
	ErrReportJump      ErrCode = "rtcp_report_jump"
	ErrTruncatedTail   ErrCode = "truncated_tail"
	ErrWALCorrupt      ErrCode = "wal_corrupt"
	ErrRollback        ErrCode = "rollback"
	ErrReplay          ErrCode = "replay_failed"
	ErrFaultInjected   ErrCode = "fault_injected"
	ErrTimeout         ErrCode = "timeout"
	ErrQueueClosed     ErrCode = "queue_closed"
	ErrLimitExceeded   ErrCode = "limit_exceeded"
	ErrDurationTooLong ErrCode = "duration_too_long"
)

// MediaError is the single error type shared across subsystems. It records
// the failing stage, the RTP sequence number closest to the failure, a
// stable code and whether the pipeline can keep producing output.
type MediaError struct {
	Stage       Stage
	Code        ErrCode
	Seq         uint16
	Recoverable bool
	Detail      string
	Cause       error
}

// Error implements error.
func (e *MediaError) Error() string {
	rec := "fatal"
	if e.Recoverable {
		rec = "recoverable"
	}
	s := fmt.Sprintf("%s/%s(%s, seq=%d): %s", e.Stage, e.Code, rec, e.Seq, e.Detail)
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *MediaError) Unwrap() error { return e.Cause }

// NewError builds a MediaError.
func NewError(stage Stage, code ErrCode, seq uint16, recoverable bool, detail string) *MediaError {
	return &MediaError{Stage: stage, Code: code, Seq: seq, Recoverable: recoverable, Detail: detail}
}

// WrapError builds a MediaError wrapping a lower-level error.
func WrapError(stage Stage, code ErrCode, seq uint16, recoverable bool, detail string, cause error) *MediaError {
	return &MediaError{Stage: stage, Code: code, Seq: seq, Recoverable: recoverable, Detail: detail, Cause: cause}
}

// IsCode reports whether err (or any wrapped error) is a MediaError with code.
func IsCode(err error, code ErrCode) bool {
	for err != nil {
		if me, ok := err.(*MediaError); ok && me.Code == code {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
