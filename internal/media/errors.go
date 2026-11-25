package media

import (
	"errors"
	"fmt"
)

// Stage identifies the pipeline phase that produced an error.
type Stage string

const (
	StageClock    Stage = "clock"
	StageIngest   Stage = "ingest"
	StageIndex    Stage = "keyindex"
	StageQueue    Stage = "bounded-queue"
	StageAudio    Stage = "audio"
	StageMux      Stage = "fmp4"
	StageWAL      Stage = "wal"
	StageRecover  Stage = "recover"
	StageManifest Stage = "manifest"
	StageMetrics  Stage = "metrics"
	StageDemo     Stage = "demo"
)

// Kind is a stable, machine-readable failure class.
type Kind string

const (
	KindInvalidArgument Kind = "invalid_argument"
	KindMissingClock    Kind = "missing_clock_declaration"
	KindUnknownCodec    Kind = "unknown_codec"
	KindOutOfBounds     Kind = "offset_out_of_bounds"
	KindTruncated       Kind = "truncated_element"
	KindTimeline        Kind = "timeline_discontinuity"
	KindDuplicate       Kind = "duplicate_timestamp"
	KindOutOfOrder      Kind = "out_of_order_timestamp"
	KindGap             Kind = "timeline_gap"
	KindWrap            Kind = "timestamp_wraparound"
	KindStaleIndex      Kind = "stale_keyframe_index"
	KindResource        Kind = "resource_limit"
	KindCanceled        Kind = "canceled"
	KindInterrupted     Kind = "interrupted"
	KindHashMismatch    Kind = "hash_mismatch"
	KindValidation      Kind = "validation_failed"
	KindPathTraversal   Kind = "path_traversal"
)

// Error is the shared error model. Offset is -1 when a byte offset is not
// meaningful. PTS is present only when HasPTS is true.
type Error struct {
	Stage     Stage
	Kind      Kind
	Offset    int64
	PTS       int64
	HasPTS    bool
	Retryable bool
	Err       error
}

func E(stage Stage, kind Kind, offset int64, err error) *Error {
	return &Error{Stage: stage, Kind: kind, Offset: offset, Err: err}
}

func (e *Error) WithPTS(pts int64) *Error {
	e.PTS = pts
	e.HasPTS = true
	return e
}

func (e *Error) WithRetryable() *Error {
	e.Retryable = true
	return e
}

func (e *Error) Error() string {
	pts := ""
	if e.HasPTS {
		pts = fmt.Sprintf(" pts=%d", e.PTS)
	}
	retry := ""
	if e.Retryable {
		retry = " retryable=true"
	}
	if e.Err != nil {
		return fmt.Sprintf("stage=%s kind=%s offset=%d%s%s: %v",
			e.Stage, e.Kind, e.Offset, pts, retry, e.Err)
	}
	return fmt.Sprintf("stage=%s kind=%s offset=%d%s%s",
		e.Stage, e.Kind, e.Offset, pts, retry)
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode maps the error model to the terminal contract.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var me *Error
	if errors.As(err, &me) {
		switch {
		case me.Retryable:
			return 75
		case me.Kind == KindResource || me.Stage == StageQueue && me.Kind == KindResource:
			return 3
		case me.Kind == KindInterrupted:
			return 75
		default:
			return 2
		}
	}
	return 3
}

// AsError exposes the typed media error when present.
func AsError(err error) (*Error, bool) {
	var me *Error
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}
