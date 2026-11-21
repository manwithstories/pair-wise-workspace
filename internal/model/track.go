// Package model holds the shared media, sample, mapping and error types used
// by every stage of the remux pipeline.
package model

import (
	"errors"
	"fmt"
)

// MediaType is the media kind of a track.
type MediaType string

const (
	MediaVideo MediaType = "video"
	MediaAudio MediaType = "audio"
)

// Track describes one input track and, after mapping, its target identity.
type Track struct {
	InputID        uint32
	TargetID       uint32
	MediaType      MediaType
	HandlerType    string
	Codec          string
	Timescale      uint32
	Language       string
	Width          uint16
	Height         uint16
	ChannelCount   uint16
	SampleSizeBits uint16
	SampleRate     uint32
	SampleEntry    []byte // complete stsd sample-entry box, copied verbatim
	SampleCount    int
	FirstDTS       uint64
	LastDTS        uint64
	TrackDuration  uint64

	// Byte offsets of the first sample-size entry and first chunk-offset
	// entry. They let validation report the exact table byte that is invalid.
	SizeTableOffset  int64
	SizeTableFixed   bool
	ChunkTableOffset int64
}

// SampleRef is a zero-copy reference to a sample inside the input file.
type SampleRef struct {
	TrackIndex int
	TrackID    uint32
	Index      int // sample number within the track, zero based
	Offset     int64
	Size       uint32
	DTS        uint64
	Duration   uint32
	CTSOffset  int64 // presentation offset from DTS
	Sync       bool
}

// Sample carries one compressed sample payload and its original timing.
type Sample struct {
	TrackIndex int
	TrackID    uint32
	Index      int
	Offset     int64
	DTS        uint64
	Duration   uint32
	CTS        int64
	Sync       bool
	Data       []byte
}

// Boundary identifies the last fully processed sample and its output byte
// offset. It is reported by streaming and recovery errors.
type Boundary struct {
	SampleIndex int
	TrackIndex  int
	TrackID     uint32
	TrackSample int
	OutputBytes int64
}

// Stage names the pipeline stage that produced an error.
type Stage string

const (
	StageDemux    Stage = "demux"
	StageMapping  Stage = "mapping"
	StageStream   Stage = "stream"
	StageMux      Stage = "mux"
	StageJournal  Stage = "journal"
	StageRecovery Stage = "recovery"
	StageVerify   Stage = "verify"
)

// Error codes are stable, machine-readable classifiers.
const (
	CodeTruncated    = "truncated"
	CodeBoxOverrun   = "box_overrun"
	CodeSampleTable  = "sample_table"
	CodeUnknownBox   = "unknown_box"
	CodeMissingBox   = "missing_box"
	CodeUnsupported  = "unsupported"
	CodeMapping      = "mapping"
	CodeTimebase     = "timebase"
	CodeNonMonotonic = "non_monotonic"
	CodeOverflow     = "overflow"
	CodeStream       = "stream"
	CodeCanceled     = "canceled"
	CodeConsumerGone = "consumer_gone"
	CodeWrite        = "write"
	CodeJournal      = "journal"
	CodeRecovery     = "recovery"
	CodeIntegrity    = "integrity"
	CodePath         = "path"
)

// Error is the single error shape shared by all stages. Offset is the first
// byte that could not be parsed, or -1 when the error is not byte-local.
type Error struct {
	Stage       Stage
	Code        string
	Offset      int64
	TrackID     uint32
	Recoverable bool
	Boundary    *Boundary
	Message     string
	Cause       error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := fmt.Sprintf("stage=%s offset=%d track=%d recoverable=%t code=%s",
		e.Stage, e.Offset, e.TrackID, e.Recoverable, e.Code)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

// NewError builds a pipeline error with an explicit byte locator.
func NewError(stage Stage, code string, offset int64, track uint32, recoverable bool, format string, args ...interface{}) *Error {
	return &Error{
		Stage: stage, Code: code, Offset: offset, TrackID: track,
		Recoverable: recoverable, Message: fmt.Sprintf(format, args...),
	}
}

// WrapError attaches pipeline context to an existing error.
func WrapError(stage Stage, code string, offset int64, track uint32, recoverable bool, cause error, format string, args ...interface{}) *Error {
	e := NewError(stage, code, offset, track, recoverable, format, args...)
	e.Cause = cause
	return e
}

// ExitCode maps an error to the CLI contract:
// 0 success, 2 invalid input/mapping, 3 recoverable processing failure,
// 4 journal or output integrity failure.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var pe *Error
	if !errors.As(err, &pe) {
		return 3
	}
	switch pe.Code {
	case CodeIntegrity, CodeJournal:
		return 4
	case CodeStream, CodeWrite, CodeCanceled, CodeConsumerGone,
		CodeRecovery:
		return 3
	default:
		return 2
	}
}
