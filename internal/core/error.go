package core

import (
	"errors"
	"fmt"
)

// Stage identifies the pipeline stage that produced an error.
type Stage string

const (
	StageCLI       Stage = "cli"
	StageManifest  Stage = "manifest"
	StageTransport Stage = "transport"
	StagePipeline  Stage = "pipeline"
	StageDemux     Stage = "demux"
	StageMapping   Stage = "mapping"
	StageRemux     Stage = "remux"
	StageIndex     Stage = "index"
	StageFixture   Stage = "fixture"
	StageInvariant Stage = "invariant"
)

// Process exit codes required by FieldPost.
const (
	ExitOK            = 0
	ExitUsage         = 2
	ExitUnrecoverable = 3
	ExitInvariant     = 4
)

// Error is the common structured error model. Offset is a byte, line, XML,
// RTP sequence or container offset according to Stage and Source.
type Error struct {
	Stage     Stage  `json:"stage"`
	Source    string `json:"source,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	Segment   string `json:"segment,omitempty"`
	Retryable bool   `json:"retryable"`
	Code      int    `json:"exit_code"`
	Message   string `json:"message"`
	Err       error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	s := fmt.Sprintf("%s: %s", e.Stage, e.Message)
	if e.Source != "" {
		s += " source=" + e.Source
	}
	if e.Segment != "" {
		s += " segment=" + e.Segment
	}
	if e.Offset != 0 {
		s += fmt.Sprintf(" offset=%d", e.Offset)
	}
	s += fmt.Sprintf(" retryable=%t", e.Retryable)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// New constructs a structured error.
func New(stage Stage, code int, retryable bool, format string, args ...any) *Error {
	return &Error{Stage: stage, Code: code, Retryable: retryable, Message: fmt.Sprintf(format, args...)}
}

// Wrap wraps err with structured location fields.
func Wrap(stage Stage, code int, retryable bool, err error, format string, args ...any) *Error {
	return &Error{Stage: stage, Code: code, Retryable: retryable, Message: fmt.Sprintf(format, args...), Err: err}
}

// ManifestError is a non-retryable manifest/argument rejection (exit 2).
func ManifestError(source string, offset int64, format string, args ...any) *Error {
	return &Error{Stage: StageManifest, Source: source, Offset: offset, Code: ExitUsage,
		Retryable: false, Message: fmt.Sprintf(format, args...)}
}

// UsageError reports invalid command-line use (exit 2).
func UsageError(format string, args ...any) *Error {
	return &Error{Stage: StageCLI, Code: ExitUsage, Retryable: false,
		Message: fmt.Sprintf(format, args...)}
}

// UnrecoverableError reports a segment that must be isolated (exit 3).
func UnrecoverableError(stage Stage, source, segment string, offset int64, format string, args ...any) *Error {
	return &Error{Stage: stage, Source: source, Segment: segment, Offset: offset,
		Code: ExitUnrecoverable, Retryable: false, Message: fmt.Sprintf(format, args...)}
}

// InvariantError reports a hash, memory or timeline failure (exit 4).
func InvariantError(stage Stage, source string, offset int64, format string, args ...any) *Error {
	return &Error{Stage: stage, Source: source, Offset: offset, Code: ExitInvariant,
		Retryable: false, Message: fmt.Sprintf(format, args...)}
}

// ExitCode maps an error to the process contract. Unknown errors default to 1.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var fe *Error
	if errors.As(err, &fe) && fe.Code != 0 {
		return fe.Code
	}
	return 1
}

// AsError returns the structured error if present.
func AsError(err error) (*Error, bool) {
	var fe *Error
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}
