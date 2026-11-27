// Package adapter defines the versioned contracts between avcore and the
// external media toolchain (ffprobe/ffmpeg). Every adapter response carries
// an adapter version, a stable error code and the source location the event
// originated from, so downstream subsystems can reason about failures without
// parsing free-form tool output.
package adapter

import (
	"context"

	"avcore/internal/clock"
)

// Version identifies a concrete adapter generation. Bumping it invalidates
// journal fingerprints from older generations, which is the intended
// compatibility boundary.
const Version = "ffmpeg_v1"

// ErrorCode is the stable, machine-readable failure taxonomy shared by every
// subsystem. Numeric values are part of the journal/audit contract.
type ErrorCode string

const (
	CodeNone           ErrorCode = ""
	CodeBadArgument    ErrorCode = "E_BAD_ARGUMENT"
	CodePathRejected   ErrorCode = "E_PATH_REJECTED"
	CodeSymlink        ErrorCode = "E_SYMLINK_INPUT"
	CodeUnsupported    ErrorCode = "E_UNSUPPORTED_FORMAT"
	CodeProbeFailed    ErrorCode = "E_PROBE_FAILED"
	CodeBadTimestamp   ErrorCode = "E_BAD_TIMESTAMP"
	CodeTruncated      ErrorCode = "E_TRUNCATED_RECORD"
	CodeOverlap        ErrorCode = "E_OVERLAPPING_CUE"
	CodeOversize       ErrorCode = "E_CAPTION_OVERSIZE"
	CodeEncodeFailed   ErrorCode = "E_ENCODE_FAILED"
	CodeMissingSegment ErrorCode = "E_MISSING_SEGMENT"
	CodeHashMismatch   ErrorCode = "E_HASH_MISMATCH"
	CodeInterrupted    ErrorCode = "E_INTERRUPTED"
	CodeInternal       ErrorCode = "E_INTERNAL"
)

// Source locates an event inside an input: file path plus 1-based line and
// byte offset (both zero when not applicable, e.g. binary probe events).
type Source struct {
	File   string `json:"file"`
	Line   int    `json:"line,omitempty"`
	Offset int64  `json:"offset,omitempty"`
}

// Error is the structured error model used across all subsystems.
type Error struct {
	Code     ErrorCode `json:"code"`
	Message  string    `json:"message"`
	Source   Source    `json:"source"`
	Recoverable bool   `json:"recoverable"`
}

func (e *Error) Error() string {
	if e.Source.Line > 0 {
		return string(e.Code) + ": " + e.Message + " (" + e.Source.File +
			":" + itoa(e.Source.Line) + " byte=" + itoa64(e.Source.Offset) + ")"
	}
	return string(e.Code) + ": " + e.Message
}

// NewError builds a structured error.
func NewError(code ErrorCode, msg string, src Source) *Error {
	return &Error{Code: code, Message: msg, Source: src, Recoverable: false}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

// MediaInfo is the normalized result of probing a source container.
type MediaInfo struct {
	Adapter       string         `json:"adapter"`
	Path          string         `json:"path"`
	FormatName    string         `json:"format_name"`
	DurationTicks int64          `json:"duration_ticks"`
	Size          int64          `json:"size"`
	Video         *VideoStream   `json:"video,omitempty"`
	Audio         *AudioStream   `json:"audio,omitempty"`
}

// VideoStream describes the single video track avcore processes.
type VideoStream struct {
	Index       int        `json:"index"`
	Codec       string     `json:"codec"`
	Width       int        `json:"width"`
	Height      int        `json:"height"`
	FrameRate   clock.Rate `json:"frame_rate"`
	HasBFrames  bool       `json:"has_b_frames"`
}

// AudioStream describes the optional audio track.
type AudioStream struct {
	Index      int    `json:"index"`
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

// Keyframe is a probe event positioned on the shared 90 kHz time base.
type Keyframe struct {
	TimeTicks int64 `json:"time_ticks"`
}

// ProbeAdapter is implemented by each versioned ffprobe adapter generation.
type ProbeAdapter interface {
	AdapterVersion() string
	// Inspect parses container metadata into normalized MediaInfo.
	Inspect(ctx context.Context, path string) (*MediaInfo, error)
	// Keyframes lists video keyframe positions on the 90 kHz time base.
	Keyframes(ctx context.Context, path string) ([]Keyframe, error)
}

// EncodeRequest fully describes one ffmpeg transcode/segmentation invocation.
type EncodeRequest struct {
	Input       string
	StartTicks  int64
	DurationTicks int64
	TargetBPS   int
	Width       int
	Height      int
	FrameRate   clock.Rate
	Output      string // final path; adapter writes a sibling temp file first
}

// EncodeResult reports what an encode actually produced.
type EncodeResult struct {
	Output     string `json:"output"`
	Bytes      int64  `json:"bytes"`
	DurationTicks int64 `json:"duration_ticks"`
	ActualBPS  int    `json:"actual_bps"`
}

// EncodeAdapter is implemented by each versioned ffmpeg adapter generation.
type EncodeAdapter interface {
	AdapterVersion() string
	Encode(ctx context.Context, req EncodeRequest) (*EncodeResult, error)
}
