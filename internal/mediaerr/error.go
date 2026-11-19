// Package mediaerr defines the structured error model shared by every pipeline
// stage. Every error carries the track, RTP sequence number, RTP timestamp and
// the stage that produced it so faults can be located from a quantified report.
package mediaerr

import (
	"errors"
	"fmt"
)

// Stage identifies the pipeline stage that emitted an error.
type Stage string

// Pipeline stages, ordered the same way data flows through the system.
const (
	StageParse     Stage = "parse"
	StageReorder   Stage = "reorder"
	StageClock     Stage = "clock"
	StageResample  Stage = "resample"
	StageLayout    Stage = "layout"
	StageLoudness  Stage = "loudness"
	StageIndex     Stage = "index"
	StageContainer Stage = "container"
	StagePipeline  Stage = "pipeline"
)

// Kind classifies an error so callers can react without parsing messages.
type Kind string

// Well-known error kinds.
const (
	KindTruncated       Kind = "truncated_packet"
	KindInvalidHeader   Kind = "invalid_rtp_header"
	KindInvalidRTCP     Kind = "invalid_rtcp"
	KindPayloadLength   Kind = "payload_length"
	KindSequenceGap     Kind = "sequence_gap"
	KindDuplicate       Kind = "duplicate_conflict"
	KindLate            Kind = "late_packet"
	KindTimestampJump   Kind = "timestamp_anomaly"
	KindLayout          Kind = "unknown_layout"
	KindIncompleteFrame Kind = "incomplete_frame"
	KindBufferLimit     Kind = "buffer_limit"
	KindIndexOutOfRange Kind = "index_out_of_range"
	KindNoKeyframe      Kind = "no_keyframe"
	KindTruncatedBox    Kind = "truncated_container"
	KindCanceled        Kind = "canceled"
	KindTimeout         Kind = "consumer_timeout"
	KindInvalidInput    Kind = "invalid_input"
)

// Error is the structured error type used across the media pipeline.
type Error struct {
	Stage    Stage
	Kind     Kind
	Track    uint32
	Sequence uint16
	// HasSeq reports whether Sequence is meaningful.
	HasSeq bool
	// RTPTimestamp is the raw 32-bit RTP timestamp; HasTS marks validity.
	RTPTimestamp uint32
	HasTS        bool
	Offset       int64
	HasOffset    bool
	Message      string
	Cause        error
}

// Error implements the error interface.
func (e *Error) Error() string {
	s := string(e.Stage) + "/" + string(e.Kind)
	if e.Track != 0 {
		s += fmt.Sprintf(" track=%d", e.Track)
	}
	if e.HasSeq {
		s += fmt.Sprintf(" seq=%d", e.Sequence)
	}
	if e.HasTS {
		s += fmt.Sprintf(" rtp_ts=%d", e.RTPTimestamp)
	}
	if e.HasOffset {
		s += fmt.Sprintf(" offset=%d", e.Offset)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// Unwrap exposes the wrapped cause for errors.Is/errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// New builds a structured error with the given stage and kind.
func New(stage Stage, kind Kind, msg string) *Error {
	return &Error{Stage: stage, Kind: kind, Message: msg}
}

// Wrap attaches a cause to a structured error.
func Wrap(stage Stage, kind Kind, msg string, cause error) *Error {
	return &Error{Stage: stage, Kind: kind, Message: msg, Cause: cause}
}

// WithTrack annotates the track id and returns the same error.
func (e *Error) WithTrack(track uint32) *Error { e.Track = track; return e }

// WithSeq annotates an RTP sequence number and returns the same error.
func (e *Error) WithSeq(seq uint16) *Error { e.Sequence = seq; e.HasSeq = true; return e }

// WithTS annotates an RTP timestamp and returns the same error.
func (e *Error) WithTS(ts uint32) *Error { e.RTPTimestamp = ts; e.HasTS = true; return e }

// WithOffset annotates a stream byte offset and returns the same error.
func (e *Error) WithOffset(off int64) *Error { e.Offset = off; e.HasOffset = true; return e }

// AsError extracts a *mediaerr.Error from any error, wrapping unknown errors
// under the supplied stage so callers always get a structured value.
func AsError(stage Stage, err error) *Error {
	if err == nil {
		return nil
	}
	var me *Error
	if errors.As(err, &me) {
		return me
	}
	return &Error{Stage: stage, Kind: KindInvalidInput, Message: err.Error(), Cause: err}
}

// Sentinel errors used for cancellation handling.
var (
	// ErrCanceled is returned once the pipeline context is canceled.
	ErrCanceled = New(StagePipeline, KindCanceled, "pipeline canceled")
	// ErrBufferFull is returned when the byte or packet cap is reached.
	ErrBufferFull = New(StagePipeline, KindBufferLimit, "buffer limit exceeded")
	// ErrTimeout is returned when a slow consumer exceeds the deadline.
	ErrTimeout = New(StagePipeline, KindTimeout, "consumer deadline exceeded")
)
