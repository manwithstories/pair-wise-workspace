// Package media: serialisable error model shared by every stage.
//
// Every failure that can be attributed to input bytes carries the byte
// offset of the first anomalous byte and the track number it belongs to,
// so the terminal output and the provenance record agree.
package media

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable, machine-readable failure classification.
type Code string

const (
	// Container layer.
	CodeTruncatedMdat    Code = "TRUNCATED_MDAT"
	CodeInvalidBoxLength Code = "INVALID_BOX_LENGTH"
	CodeBoxBeyondEOF     Code = "BOX_BEYOND_EOF"
	CodeOversizedBox     Code = "OVERSIZED_BOX"
	CodeMissingBox       Code = "MISSING_BOX"
	CodeUnsupportedTrack Code = "UNSUPPORTED_TRACK"
	CodeSampleBeyondMdat Code = "SAMPLE_BEYOND_MDAT"
	CodeNoAudioTrack     Code = "NO_AUDIO_TRACK"

	// Time base layer.
	CodeTimestampRegression Code = "TIMESTAMP_REGRESSION"
	CodeTimestampOverflow   Code = "TIMESTAMP_OVERFLOW"

	// Codec layer.
	CodeUnsupportedCodec Code = "UNSUPPORTED_CODEC"
	CodeInvalidConfig    Code = "INVALID_CODEC_CONFIG"

	// Engine layer.
	CodeUnalignedPCM      Code = "UNALIGNED_PCM"
	CodeChannelsInvalid   Code = "INVALID_CHANNELS"
	CodeInvalidSampleRate Code = "INVALID_SAMPLE_RATE"
	CodeLoudnessInvalid   Code = "INVALID_LOUDNESS"
	CodeBlockTooLong      Code = "BLOCK_TOO_LONG"
	CodeMemoryLimit       Code = "MEMORY_LIMIT"

	// Store / lifecycle layer.
	CodeCheckpointConflict Code = "CHECKPOINT_CONFLICT"
	CodeCheckpointCorrupt  Code = "CHECKPOINT_CORRUPT"
	CodeAtomicWrite        Code = "ATOMIC_WRITE_FAILED"
	CodeOutputVerification Code = "OUTPUT_VERIFICATION_FAILED"
	CodeWorkDirEscape      Code = "WORKDIR_ESCAPE"
	CodeInterrupted        Code = "INTERRUPTED"

	// Command layer.
	CodeUsage Code = "USAGE"
)

// Exit codes contract.
const (
	ExitOK         = 0
	ExitInput      = 2
	ExitCheckpoint = 3
	ExitProcessing = 4
)

// Error is the serialised form of any mediacore failure.
type Error struct {
	Code    Code   `json:"error_code"`
	Offset  int64  `json:"offset"`
	Track   int    `json:"track"`
	Stage   string `json:"stage,omitempty"`
	Message string `json:"message,omitempty"`
	// HasOffset distinguishes a genuine offset 0 from "unknown".
	HasOffset bool `json:"has_offset,omitempty"`
	cause     error
}

// NewError builds an Error. Pass offset < 0 when the position is unknown.
func NewError(code Code, offset int64, track int, format string, args ...any) *Error {
	e := &Error{Code: code, Track: track, Offset: offset, Message: fmt.Sprintf(format, args...)}
	if offset >= 0 {
		e.HasOffset = true
	} else {
		e.Offset = 0
	}
	return e
}

// Wrap attaches an underlying cause.
func (e *Error) Wrap(err error) *Error {
	e.cause = err
	return e
}

// WithStage labels the pipeline stage that produced the error.
func (e *Error) WithStage(stage string) *Error {
	e.Stage = stage
	return e
}

// Unwrap exposes the underlying cause.
func (e *Error) Unwrap() error { return e.cause }

// Error implements error. The terminal form is intentionally the same
// key=value shape reported in the acceptance output.
func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "error_code=%s", e.Code)
	if e.HasOffset {
		fmt.Fprintf(&b, " offset=%d", e.Offset)
	}
	if e.Track > 0 {
		fmt.Fprintf(&b, " track=%d", e.Track)
	}
	if e.Stage != "" {
		fmt.Fprintf(&b, " stage=%s", e.Stage)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.cause != nil {
		fmt.Fprintf(&b, " (cause: %v)", e.cause)
	}
	return b.String()
}

// Line is the single-line acceptance rendering without prose.
func (e *Error) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "error_code=%s", e.Code)
	if e.HasOffset {
		fmt.Fprintf(&b, " offset=%d", e.Offset)
	}
	if e.Track > 0 {
		fmt.Fprintf(&b, " track=%d", e.Track)
	}
	return b.String()
}

// ExitCode maps a code onto the process contract.
func (e *Error) ExitCode() int {
	switch e.Code {
	case CodeUsage, CodeNoAudioTrack, CodeUnsupportedCodec, CodeInvalidConfig,
		CodeInvalidSampleRate, CodeChannelsInvalid, CodeLoudnessInvalid,
		CodeUnsupportedTrack, CodeMissingBox:
		return ExitInput
	case CodeCheckpointConflict, CodeCheckpointCorrupt:
		return ExitCheckpoint
	default:
		return ExitProcessing
	}
}

// AsError extracts a *Error from an arbitrary error chain.
func AsError(err error) (*Error, bool) {
	var me *Error
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// ExitCodeOf returns the process exit code for any error.
func ExitCodeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	if me, ok := AsError(err); ok {
		return me.ExitCode()
	}
	return ExitProcessing
}
