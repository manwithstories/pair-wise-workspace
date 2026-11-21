// Package fault defines the single error and diagnostic vocabulary shared by
// parsing, audio, streaming and journal modules. Every failure carries a
// stage, an optional track and an optional media time span so that an operator
// can locate the damaged segment without re-running the pipeline by hand.
package fault

import (
	"errors"
	"fmt"
	"strings"

	"mediamerge/internal/clock"
)

// Code identifies the failure category. Values are stable and intended for
// machine-readable terminal output and test assertions.
type Code string

const (
	CodeParseBox           Code = "PARSE_BOX"
	CodeTruncatedBox       Code = "TRUNCATED_BOX"
	CodeTruncatedPartition Code = "TRUNCATED_MXF_PARTITION"
	CodeNonMonotonic       Code = "NON_MONOTONIC_TIME"
	CodeCropOutOfBounds    Code = "CROP_OUT_OF_BOUNDS"
	CodeRotationInvalid    Code = "ROTATION_INVALID"
	CodeColorMissing       Code = "COLOR_MISSING"
	CodeColorConflict      Code = "COLOR_CONFLICT"
	CodeLayoutIncompat     Code = "LAYOUT_INCOMPATIBLE"
	CodeSampleIndivisible  Code = "SAMPLE_COUNT_INDIVISIBLE"
	CodeLoudnessTarget     Code = "LOUDNESS_TARGET"
	CodeQueueFull          Code = "QUEUE_FULL"
	CodeCanceled           Code = "CANCELED"
	CodeWriteFailed        Code = "WRITE_FAILED"
	CodeBackpressure       Code = "BACKPRESSURE"
	CodeChecksumMismatch   Code = "CHECKSUM_MISMATCH"
	CodeWALCorrupt         Code = "WAL_CORRUPT"
	CodeWALGap             Code = "WAL_GAP"
	CodeStageFailed        Code = "STAGE_FAILED"
	CodeInvalidArgument    Code = "INVALID_ARGUMENT"
)

// Stage names the pipeline phase that produced the failure.
type Stage string

const (
	StageArgs     Stage = "args"
	StageParse    Stage = "parse"
	StageResolve  Stage = "resolve"
	StageAudio    Stage = "audio"
	StageStream   Stage = "stream"
	StageJournal  Stage = "journal"
	StageCommit   Stage = "commit"
	StageValidate Stage = "validate"
)

// Class describes what a supervisor may do with the failure.
type Class int

const (
	// ClassFatal means retrying the same input cannot succeed.
	ClassFatal Class = iota
	// ClassRetryable means a transient resource condition (slow consumer,
	// cancelled context) interrupted the run and a resume may proceed.
	ClassRetryable
	// ClassCorrupt means persisted state is inconsistent and the run must
	// roll back to the last committed boundary before retrying.
	ClassCorrupt
)

func (c Class) String() string {
	switch c {
	case ClassRetryable:
		return "retryable"
	case ClassCorrupt:
		return "corrupt"
	default:
		return "fatal"
	}
}

// Error is the cross-module error. Cause is retained for errors.Is/As.
type Error struct {
	Code    Code
	Stage   Stage
	TrackID int
	// Span is nil when the failure is not tied to a media range.
	Span   *clock.Span
	Detail string
	Cause  error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Stage))
	b.WriteString(": ")
	b.WriteString(string(e.Code))
	if e.TrackID > 0 {
		fmt.Fprintf(&b, " track=%d", e.TrackID)
	}
	if e.Span != nil {
		fmt.Fprintf(&b, " range=%s", e.Span)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

// New builds an error without location. Prefer NewTrack or NewSpan.
func New(stage Stage, code Code, detail string) *Error {
	return &Error{Stage: stage, Code: code, Detail: detail}
}

// NewTrack attaches a track identifier.
func NewTrack(stage Stage, code Code, track int, detail string) *Error {
	return &Error{Stage: stage, Code: code, TrackID: track, Detail: detail}
}

// NewSpan attaches track and media range.
func NewSpan(stage Stage, code Code, track int, span clock.Span, detail string) *Error {
	return &Error{Stage: stage, Code: code, TrackID: track, Span: &span, Detail: detail}
}

// Wrap preserves a lower-level cause while fixing stage and code.
func Wrap(stage Stage, code Code, cause error, detail string) *Error {
	return &Error{Stage: stage, Code: code, Detail: detail, Cause: cause}
}

// locatedError carries an enriched clone without discarding the original
// error chain. Unwrap exposes both so errors.Is still reaches the source.
type locatedError struct {
	located  *Error
	original error
}

func (l *locatedError) Error() string { return l.located.Error() }

func (l *locatedError) Unwrap() []error { return []error{l.located, l.original} }

// Locate returns a copy of err carrying track and span when it is a *Error.
// Non-fault errors are wrapped as a stage failure so no location is lost at a
// module boundary. The original error chain is retained.
func Locate(err error, stage Stage, track int, span clock.Span) error {
	if err == nil {
		return nil
	}
	var fe *Error
	if errors.As(err, &fe) {
		clone := *fe
		if clone.TrackID == 0 {
			clone.TrackID = track
		}
		if clone.Span == nil {
			s := span
			clone.Span = &s
		}
		if clone.Stage == "" {
			clone.Stage = stage
		}
		return &locatedError{located: &clone, original: err}
	}
	return &Error{Stage: stage, Code: CodeStageFailed, TrackID: track, Span: &span, Cause: err}
}

var retryableCodes = map[Code]bool{
	CodeQueueFull:    true,
	CodeCanceled:     true,
	CodeBackpressure: true,
	CodeWriteFailed:  true,
}

var corruptCodes = map[Code]bool{
	CodeChecksumMismatch: true,
	CodeWALCorrupt:       true,
	CodeWALGap:           true,
}

// Classify maps an error to the supervisor action. Anything not personally
// understood is fatal, so an unknown failure never causes a blind retry.
func Classify(err error) Class {
	if err == nil {
		return ClassFatal
	}
	var fe *Error
	if errors.As(err, &fe) {
		if corruptCodes[fe.Code] {
			return ClassCorrupt
		}
		if retryableCodes[fe.Code] {
			return ClassRetryable
		}
	}
	if errors.Is(err, contextCanceled) {
		return ClassRetryable
	}
	return ClassFatal
}

// Severity levels for structured diagnostics.
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Diagnostic is a structured, non-panicking finding such as a colour field
// resolved by precedence or one that could not be resolved at all.
type Diagnostic struct {
	Code     Code
	Severity Severity
	Stage    Stage
	TrackID  int
	Field    string
	Chosen   string
	Rejected []string
	Span     *clock.Span
	Message  string
}

func (d Diagnostic) String() string {
	var b strings.Builder
	b.WriteString(string(d.Severity))
	b.WriteString(" ")
	b.WriteString(string(d.Code))
	if d.TrackID > 0 {
		fmt.Fprintf(&b, " track=%d", d.TrackID)
	}
	if d.Field != "" {
		fmt.Fprintf(&b, " %s", d.Field)
	}
	if d.Chosen != "" {
		fmt.Fprintf(&b, "=%s", d.Chosen)
	}
	if len(d.Rejected) > 0 {
		fmt.Fprintf(&b, " rejected=%s", strings.Join(d.Rejected, ","))
	}
	if d.Span != nil {
		fmt.Fprintf(&b, " range=%s", d.Span)
	}
	if d.Message != "" {
		fmt.Fprintf(&b, " (%s)", d.Message)
	}
	return b.String()
}

// Report collects diagnostics. It is not safe for concurrent use.
type Report struct {
	Items []Diagnostic
}

func (r *Report) Add(d Diagnostic) { r.Items = append(r.Items, d) }

// Errors returns only error-severity items.
func (r *Report) Errors() []Diagnostic {
	var out []Diagnostic
	for _, d := range r.Items {
		if d.Severity == SeverityError {
			out = append(out, d)
		}
	}
	return out
}

// HasErrors reports whether any finding blocks delivery.
func (r *Report) HasErrors() bool { return len(r.Errors()) > 0 }

// AsError converts the first error-severity diagnostic into a located fault,
// preserving all findings in the detail string.
func (r *Report) AsError(stage Stage) error {
	errs := r.Errors()
	if len(errs) == 0 {
		return nil
	}
	first := errs[0]
	var details []string
	for _, d := range errs {
		details = append(details, d.String())
	}
	e := &Error{
		Stage:   stage,
		Code:    first.Code,
		TrackID: first.TrackID,
		Span:    first.Span,
		Detail:  strings.Join(details, "; "),
	}
	return e
}
