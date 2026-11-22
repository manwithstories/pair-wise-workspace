// Package fault defines the single error model shared by adapters, the remux
// kernel and the journal. Every failure carries enough structured location
// data (source path, track, packet offset) that an operator can point at the
// exact byte in the exact track without re-running with extra logging.
package fault

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable, machine-readable error classification surfaced in the CLI
// and the provenance manifest.
type Code string

const (
	CodeUnknown          Code = "unknown"
	CodeUnknownVersion   Code = "unknown_version"
	CodeCorruptContainer Code = "corrupt_container"
	CodeBadIndex         Code = "bad_index"
	CodeEditList         Code = "edit_list"
	CodeNegativeEdit     Code = "negative_edit_offset"
	CodeTimestampWrap    Code = "timestamp_wrap"
	CodeDurationRange    Code = "duration_out_of_range"
	CodeTimescale        Code = "invalid_timescale"
	CodeMapping          Code = "mapping_rejected"
	CodeMissingMapping   Code = "missing_mapping"
	CodeDuplicateMapping Code = "duplicate_mapping"
	CodeCheckpoint       Code = "checkpoint_unrecoverable"
	CodeDigestMismatch   Code = "digest_mismatch"
	CodeAdapterChanged   Code = "adapter_version_changed"
	CodeVerify           Code = "verify_failed"
	CodePath             Code = "path_rejected"
	CodeNoPackets        Code = "no_packets"
)

// AllCodes lists every code the CLI may emit; used by tests to keep the
// taxonomy honest.
func AllCodes() []Code {
	return []Code{
		CodeUnknown, CodeUnknownVersion, CodeCorruptContainer, CodeBadIndex,
		CodeEditList, CodeNegativeEdit, CodeTimestampWrap, CodeDurationRange,
		CodeTimescale, CodeMapping, CodeMissingMapping, CodeDuplicateMapping,
		CodeCheckpoint, CodeDigestMismatch, CodeAdapterChanged, CodeVerify,
		CodePath, CodeNoPackets,
	}
}

// Severity distinguishes fatal asset errors from recoverable per-asset skips.
type Severity int

const (
	// Fatal aborts the current asset and is reported to the caller.
	Fatal Severity = iota
	// Recoverable lets the run continue with remaining assets.
	Recoverable
)

// Location pins an error to a concrete source position. Zero values mean
// "not applicable" and are omitted from rendering.
type Location struct {
	SourcePath string
	TrackID    uint32
	HasTrack   bool
	Offset     int64
	HasOffset  bool
	Sequence   uint32
	HasSeq     bool
	AssetID    string
	Adapter    string
}

// WithPath sets the source path.
func (l Location) WithPath(p string) Location { l.SourcePath = p; return l }

// WithTrack records a track id.
func (l Location) WithTrack(id uint32) Location {
	l.TrackID, l.HasTrack = id, true
	return l
}

// WithOffset records a byte offset into the source.
func (l Location) WithOffset(o int64) Location { l.Offset, l.HasOffset = o, true; return l }

// WithSequence records a packet sequence number (RTP or sample index).
func (l Location) WithSequence(s uint32) Location { l.Sequence, l.HasSeq = s, true; return l }

// String renders a compact, log-friendly locator.
func (l Location) String() string {
	var b strings.Builder
	if l.AssetID != "" {
		b.WriteString("asset=")
		b.WriteString(l.AssetID)
		b.WriteString(" ")
	}
	if l.SourcePath != "" {
		b.WriteString("path=")
		b.WriteString(l.SourcePath)
		b.WriteString(" ")
	}
	if l.HasTrack {
		fmt.Fprintf(&b, "track=%d ", l.TrackID)
	}
	if l.HasOffset {
		fmt.Fprintf(&b, "offset=%d ", l.Offset)
	}
	if l.HasSeq {
		fmt.Fprintf(&b, "seq=%d ", l.Sequence)
	}
	if l.Adapter != "" {
		b.WriteString("adapter=")
		b.WriteString(l.Adapter)
	}
	return strings.TrimSpace(b.String())
}

// Error is the structured error produced throughout the pipeline.
type Error struct {
	Code     Code
	Severity Severity
	Loc      Location
	Op       string
	Message  string
	Recovery string
	Cause    error
}

// New builds a Fatal error with the given code and message.
func New(code Code, loc Location, format string, args ...any) *Error {
	return &Error{Code: code, Severity: Fatal, Loc: loc, Message: fmt.Sprintf(format, args...)}
}

// Recoverable builds a non-fatal error that should not abort the whole run.
func NewRecoverable(code Code, loc Location, format string, args ...any) *Error {
	return &Error{Code: code, Severity: Recoverable, Loc: loc, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches an underlying cause.
func (e *Error) Wrap(err error) *Error { e.Cause = err; return e }

// WithOp labels the failing operation (e.g. "mp4.ParseTrak").
func (e *Error) WithOp(op string) *Error { e.Op = op; return e }

// WithRecovery documents the action taken or expected.
func (e *Error) WithRecovery(format string, args ...any) *Error {
	e.Recovery = fmt.Sprintf(format, args...)
	return e
}

// WithLocation replaces the locator.
func (e *Error) WithLocation(loc Location) *Error { e.Loc = loc; return e }

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "error_code=%s", e.Code)
	if loc := e.Loc.String(); loc != "" {
		b.WriteString(" ")
		b.WriteString(loc)
	}
	if e.Op != "" {
		b.WriteString(" op=")
		b.WriteString(e.Op)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Cause != nil {
		b.WriteString(" [cause: ")
		b.WriteString(e.Cause.Error())
		b.WriteString("]")
	}
	if e.Recovery != "" {
		b.WriteString(" [recovery: ")
		b.WriteString(e.Recovery)
		b.WriteString("]")
	}
	return b.String()
}

// Unwrap exposes the cause chain to errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }

// Is compares by stable code so callers can branch without matching strings.
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return e.Code == t.Code
	}
	return false
}

// As extracts a structured Error from an arbitrary error chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// CodeOf returns the stable code for err, defaulting to CodeUnknown.
func CodeOf(err error) Code {
	if e, ok := As(err); ok {
		return e.Code
	}
	return CodeUnknown
}

// ExitCode maps an error to the CLI contract:
// 2 input/mapping, 3 remux/verify, 4 checkpoint, 1 otherwise.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch CodeOf(err) {
	case CodeUnknownVersion, CodeCorruptContainer, CodeBadIndex, CodeEditList,
		CodeNegativeEdit, CodeTimestampWrap, CodeDurationRange, CodeTimescale,
		CodeMapping, CodeMissingMapping, CodeDuplicateMapping, CodePath, CodeNoPackets:
		return 2
	case CodeVerify, CodeDigestMismatch:
		return 3
	case CodeCheckpoint, CodeAdapterChanged:
		return 4
	default:
		return 1
	}
}

// Aggregate collects per-asset errors while letting a batch continue.
type Aggregate struct {
	Errors []*Error
}

// Add appends e. It returns true when the aggregate now holds a fatal error.
func (a *Aggregate) Add(e *Error) bool {
	if e == nil {
		return false
	}
	a.Errors = append(a.Errors, e)
	return e.Severity == Fatal
}

// Recovered counts recoverable entries.
func (a *Aggregate) Recovered() int {
	n := 0
	for _, e := range a.Errors {
		if e.Severity == Recoverable {
			n++
		}
	}
	return n
}

// Fatal reports whether any fatal error was recorded.
func (a *Aggregate) Fatal() bool {
	for _, e := range a.Errors {
		if e.Severity == Fatal {
			return true
		}
	}
	return false
}

func (a *Aggregate) Error() string {
	parts := make([]string, 0, len(a.Errors))
	for _, e := range a.Errors {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "; ")
}

// Unwrap returns all entries so errors.Is can scan them.
func (a *Aggregate) Unwrap() []error {
	out := make([]error, 0, len(a.Errors))
	for _, e := range a.Errors {
		out = append(out, e)
	}
	return out
}
