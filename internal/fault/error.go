// Package fault defines the error model shared by every stage of the
// archive pipeline. Every error carries the stage in which it was detected,
// the absolute source byte offset when one is known, and the Matroska track
// number when the failure is track-local. That information is what makes a
// damaged archive actionable: it points an engineer at the exact bytes.
package fault

import (
	"errors"
	"fmt"
	"strings"
)

// Stage names the pipeline stage that detected an error.
type Stage string

const (
	// StageOpen covers filesystem and input-root checks.
	StageOpen Stage = "open"
	// StageParse covers EBML element parsing and structural limits.
	StageParse Stage = "parse"
	// StageDemux covers block decoding and lacing.
	StageDemux Stage = "demux"
	// StageMap covers track validation and time mapping.
	StageMap Stage = "map"
	// StageQueue covers bounded-queue delivery and backpressure.
	StageQueue Stage = "queue"
	// StageRemux covers MP4 box construction and writing.
	StageRemux Stage = "remux"
	// StageVerify covers re-reading the output and hashing it.
	StageVerify Stage = "verify"
	// StageIndex covers Cues generation and lookup.
	StageIndex Stage = "index"
	// StageIO covers output I/O failures unrelated to the input format.
	StageIO Stage = "io"
)

// Class describes what a caller may do about an error.
type Class int

const (
	// ClassCorrupt means the input bytes are malformed. Exit code 2.
	ClassCorrupt Class = iota + 1
	// ClassUnsupported means the input is well-formed but uses a coding this
	// tool refuses to remux. Exit code 2 with a distinct message.
	ClassUnsupported
	// ClassCancelled means the context was cancelled or a consumer stopped.
	ClassCancelled
	// ClassRuntime means a local runtime or output failure. Exit code 3.
	ClassRuntime
)

var (
	// ErrTruncated marks an element whose declared end lies beyond the input.
	ErrTruncated = errors.New("truncated input")
	// ErrOversized marks a length or size that cannot fit its parent.
	ErrOversized = errors.New("declared size exceeds parent or input")
	// ErrMalformed marks structurally invalid bytes.
	ErrMalformed = errors.New("malformed element")
	// ErrUnknownTrack marks a block referencing an undeclared track number.
	ErrUnknownTrack = errors.New("block references unknown track")
	// ErrUnsupportedCodec marks a codec without a documented MP4 mapping.
	ErrUnsupportedCodec = errors.New("unsupported codec")
	// ErrCorruptBlock marks a block that fails validation after parsing.
	ErrCorruptBlock = errors.New("corrupt block")
	// ErrCancelled marks cancellation or a closed consumer.
	ErrCancelled = errors.New("cancelled")
	// ErrOutput marks an output write failure.
	ErrOutput = errors.New("output failure")
	// ErrBoundary marks a recovery boundary that could not be proven.
	ErrBoundary = errors.New("recovery boundary is not provable")
)

// Error is the common pipeline error.
type Error struct {
	Stage  Stage
	Offset int64 // absolute source byte offset, -1 when unknown
	Track  uint64
	// Element names the EBML element or MP4 box involved, if any.
	Element string
	// Op names the operation for runtime errors.
	Op  string
	Err error
}

// New builds an Error with an unknown offset.
func New(stage Stage, err error) *Error {
	return &Error{Stage: stage, Offset: -1, Err: err}
}

// At builds an Error anchored at a source byte offset.
func At(stage Stage, offset int64, err error) *Error {
	return &Error{Stage: stage, Offset: offset, Err: err}
}

// AtElement builds an Error anchored at a source byte offset and element.
func AtElement(stage Stage, offset int64, element string, err error) *Error {
	return &Error{Stage: stage, Offset: offset, Element: element, Err: err}
}

// OnTrack annotates an error with a Matroska track number.
func (e *Error) OnTrack(track uint64) *Error {
	e.Track = track
	return e
}

// InElement annotates an error with the EBML element name.
func (e *Error) InElement(name string) *Error {
	e.Element = name
	return e
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("stage=")
	b.WriteString(string(e.Stage))
	if e.Op != "" {
		b.WriteString(" op=")
		b.WriteString(e.Op)
	}
	if e.Element != "" {
		b.WriteString(" element=")
		b.WriteString(e.Element)
	}
	if e.Track != 0 {
		fmt.Fprintf(&b, " track=%d", e.Track)
	}
	if e.Offset >= 0 {
		fmt.Fprintf(&b, " offset=%d", e.Offset)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the cause for errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// Class returns the error class. Unclassified stage errors count as corrupt
// when they wrap a known corruption sentinel and as runtime otherwise.
func (e *Error) Class() Class {
	switch {
	case errors.Is(e.Err, ErrUnsupportedCodec):
		return ClassUnsupported
	case errors.Is(e.Err, ErrOutput), errors.Is(e.Err, ErrCancelled):
		return ClassRuntime
	case errors.Is(e.Err, ErrTruncated), errors.Is(e.Err, ErrOversized),
		errors.Is(e.Err, ErrMalformed), errors.Is(e.Err, ErrUnknownTrack),
		errors.Is(e.Err, ErrCorruptBlock), errors.Is(e.Err, ErrBoundary):
		return ClassCorrupt
	case e.Stage == StageIO, e.Stage == StageOpen:
		return ClassRuntime
	default:
		return ClassCorrupt
	}
}

// ExitCode maps an error to the CLI exit code contract:
// 2 for corrupt or unsupported input, 3 for runtime/output errors.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var fe *Error
	if errors.As(err, &fe) {
		if fe.Class() == ClassRuntime {
			return 3
		}
		return 2
	}
	return 3
}

// IsCorrupt reports whether err is a content defect rather than a runtime
// failure.
func IsCorrupt(err error) bool {
	var fe *Error
	if !errors.As(err, &fe) {
		return false
	}
	c := fe.Class()
	return c == ClassCorrupt || c == ClassUnsupported
}

// StageOf returns the stage of err, or an empty stage when err is not a
// pipeline Error.
func StageOf(err error) Stage {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Stage
	}
	return ""
}

// OffsetOf returns the source offset carried by err, or -1.
func OffsetOf(err error) int64 {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Offset
	}
	return -1
}

// TrackOf returns the track number carried by err, or 0.
func TrackOf(err error) uint64 {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Track
	}
	return 0
}

// Wrap adds stage context to err without losing the original error.
func Wrap(stage Stage, err error) error {
	if err == nil {
		return nil
	}
	var fe *Error
	if errors.As(err, &fe) {
		return err
	}
	return New(stage, err)
}

// Skip describes a deliberately skipped byte range during recovery.
type Skip struct {
	Start  int64
	Length int64
	// Reason explains why the range was provably safe to skip.
	Reason string
	// Track is non-zero for block-level skips.
	Track uint64
}

// RecoveryError is returned when recovery consumed damage. Errors that stop
// recovery before any provable boundary are ordinary Errors.
type RecoveryError struct {
	Cause  error
	Skips  []Skip
	Stages []Stage
}

// Error implements error.
func (r *RecoveryError) Error() string {
	return fmt.Sprintf("recovered beyond %v: %d skipped segment(s)", r.Cause, len(r.Skips))
}

// Unwrap exposes the original cause.
func (r *RecoveryError) Unwrap() error { return r.Cause }

// SkippedBytes returns the total number of bytes skipped.
func (r *RecoveryError) SkippedBytes() int64 {
	var n int64
	for _, s := range r.Skips {
		n += s.Length
	}
	return n
}
