// Package mediaerr defines the structured error model shared by every
// subsystem (ingest, metadata, pipeline, remux, cli). Every failure that is
// reported to the operator carries a stable machine-readable code plus the
// context needed to locate it (input path, track, byte offset).
package mediaerr

import "fmt"

// ErrorCode is a stable, machine-readable identifier for a failure class.
type ErrorCode string

const (
	// CodeInvalidArgs: invalid CLI parameters or mapping configuration (exit 1).
	CodeInvalidArgs ErrorCode = "INVALID_ARGS"
	// CodeInvalidConfig: mapping/metadata configuration conflict (exit 1).
	CodeInvalidConfig ErrorCode = "INVALID_CONFIG"
	// CodePathTraversal: a path escapes the permitted working directory (exit 1).
	CodePathTraversal ErrorCode = "PATH_TRAVERSAL"
	// CodeTruncatedBox: an MP4 box header/payload runs past EOF (exit 2).
	CodeTruncatedBox ErrorCode = "TRUNCATED_BOX"
	// CodeTruncatedKLV: an MXF KLV triplet runs past EOF (exit 2).
	CodeTruncatedKLV ErrorCode = "TRUNCATED_KLV"
	// CodeUnknownStructure: an unsupported/unknown box or MXF key where the
	// documented subset requires recognition (exit 2).
	CodeUnknownStructure ErrorCode = "UNKNOWN_STRUCTURE"
	// CodeCropOutOfBounds: crop rectangle exceeds the coded frame (exit 2).
	CodeCropOutOfBounds ErrorCode = "CROP_OUT_OF_BOUNDS"
	// CodeNegativeDuration: a sample/container duration is negative (exit 2).
	CodeNegativeDuration ErrorCode = "NEGATIVE_DURATION"
	// CodeDTSRegression: DTS moves backwards on one track (exit 2).
	CodeDTSRegression ErrorCode = "DTS_REGRESSION"
	// CodeManifestMisalign: cross-track manifest timeline drift exceeds limit (exit 2).
	CodeManifestMisalign ErrorCode = "MANIFEST_MISALIGN"
	// CodeSequenceGap: RTP/track sequence or timestamp discontinuity that cannot
	// be explained by a modular wraparound (exit 2).
	CodeSequenceGap ErrorCode = "SEQUENCE_GAP"
	// CodeBlockTooLarge: a sample block exceeds the 4 MiB ceiling (exit 2).
	CodeBlockTooLarge ErrorCode = "BLOCK_TOO_LARGE"
	// CodeCheckpointMismatch: checkpoint no longer matches input/config (exit 3).
	CodeCheckpointMismatch ErrorCode = "CHECKPOINT_MISMATCH"
	// CodeIO: underlying filesystem/IO failure (exit 2).
	CodeIO ErrorCode = "IO_FAILURE"
)

// Error is the structured error type propagated through the whole pipeline.
type Error struct {
	Code    ErrorCode `json:"code"`
	Format  string    `json:"format,omitempty"` // "mp4" | "mxf" | "pipeline" ...
	Track   string    `json:"track,omitempty"`  // track identifier, e.g. "video/0"
	Path    string    `json:"path,omitempty"`   // offending input path
	Offset  int64     `json:"offset,omitempty"` // absolute byte offset in input
	Detail  string    `json:"detail,omitempty"` // human-readable detail
	wrapped error     `json:"-"`
}

func (e *Error) Error() string {
	s := string(e.Code)
	if e.Format != "" {
		s += " format=" + e.Format
	}
	if e.Track != "" {
		s += " track=" + e.Track
	}
	if e.Path != "" {
		s += " path=" + e.Path
	}
	if e.Offset >= 0 {
		s += fmt.Sprintf(" input_offset=%d", e.Offset)
	}
	if e.Detail != "" {
		s += " detail=" + e.Detail
	}
	return s
}

// Unwrap exposes the wrapped cause for errors.Is/errors.As.
func (e *Error) Unwrap() error { return e.wrapped }

// With wraps a cause and returns the receiver, for fluent construction.
func (e *Error) With(cause error) *Error {
	e.wrapped = cause
	return e
}

// New builds a structured error. A negative offset means "not applicable".
func New(code ErrorCode, detail string) *Error {
	return &Error{Code: code, Offset: -1, Detail: detail}
}

// At attaches input location context (format, track, absolute byte offset).
func (e *Error) At(format, track string, offset int64) *Error {
	e.Format, e.Track, e.Offset = format, track, offset
	return e
}

// OnPath attaches the offending input path.
func (e *Error) OnPath(path string) *Error {
	e.Path = path
	return e
}

// ExitCode maps an error to the documented process exit code:
// 1 = invalid arguments/configuration, 2 = input/processing failure,
// 3 = checkpoint mismatch.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	se, ok := err.(*Error)
	if !ok {
		return 2
	}
	switch se.Code {
	case CodeInvalidArgs, CodeInvalidConfig, CodePathTraversal:
		return 1
	case CodeCheckpointMismatch:
		return 3
	default:
		return 2
	}
}

// Is reports whether err (or any wrapped error) carries the given code.
func Is(err error, code ErrorCode) bool {
	for err != nil {
		if se, ok := err.(*Error); ok && se.Code == code {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
