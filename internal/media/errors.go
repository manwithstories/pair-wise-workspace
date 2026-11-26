package media

import (
	"errors"
	"fmt"
	"strings"
)

// Stable sentinel kinds shared by all stages.
var (
	ErrArgument           = errors.New("invalid argument")
	ErrCRC32              = errors.New("crc32 mismatch")
	ErrTimestampOrder     = errors.New("track timestamp reversed")
	ErrSequenceOrder      = errors.New("track sequence reversed")
	ErrTransitionEndpoint = errors.New("missing transition endpoint")
	ErrTruncatedRecord    = errors.New("truncated ndjson record")
	ErrMalformedRecord    = errors.New("malformed ndjson record")
	ErrBoundaryMismatch   = errors.New("representation boundary mismatch")
	ErrDurationMismatch   = errors.New("representation duration mismatch")
	ErrInputFingerprint   = errors.New("input fingerprint mismatch")
	ErrConfigFingerprint  = errors.New("configuration fingerprint mismatch")
	ErrCheckpoint         = errors.New("invalid checkpoint")
	ErrChunkDigest        = errors.New("committed chunk digest mismatch")
	ErrInterrupted        = errors.New("recoverable interruption")
	ErrUnlocatedCorrupt   = errors.New("input corruption could not be located")
)

// Exit codes used by cmd/relay.
const (
	ExitOK               = 0
	ExitUsage            = 2
	ExitCorruptUnlocated = 3
	ExitRecoverable      = 75
)

// Meta is the minimum source location every subsystem must preserve.
type Meta struct {
	Track     string `json:"track,omitempty"`
	Rendition string `json:"rendition,omitempty"`
	RecordNo  int    `json:"record_no"`
	LineNo    int    `json:"line_no,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	PTS90k    Tick   `json:"pts90k,omitempty"`
}

// Error is the one error model used from NDJSON parsing through manifests.
// A pointer is safe to compare with errors.As and retains the first source
// position even when later stages annotate the stage trail.
type Error struct {
	Kind    error    `json:"-"`
	Stage   string   `json:"stage"`
	Stages  []string `json:"stages,omitempty"`
	Code    string   `json:"code"`
	Meta    Meta     `json:"meta"`
	Details string   `json:"details,omitempty"`
	Err     error    `json:"-"`
}

// E creates a unified error.
func E(kind error, stage string, meta Meta, cause error) *Error {
	e := &Error{Kind: kind, Stage: stage, Meta: meta, Err: cause}
	e.Stages = []string{stage}
	e.Code = CodeOf(kind)
	if cause != nil {
		e.Details = cause.Error()
	}
	return e
}

// Error renders the stable location prefix followed by the stage trail.
func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s at record=%d line=%d", e.Code, e.Meta.RecordNo, e.Meta.LineNo)
	if e.Meta.Track != "" {
		fmt.Fprintf(&b, " track=%q", e.Meta.Track)
	}
	if e.Meta.Rendition != "" {
		fmt.Fprintf(&b, " rendition=%q", e.Meta.Rendition)
	}
	if e.Meta.PTS90k != 0 || e.Kind == ErrTimestampOrder {
		fmt.Fprintf(&b, " pts90k=%d", e.Meta.PTS90k)
	}
	if len(e.Stages) > 0 {
		fmt.Fprintf(&b, " stages=%s", strings.Join(e.Stages, ">"))
	}
	if e.Details != "" {
		fmt.Fprintf(&b, ": %s", e.Details)
	}
	return b.String()
}

// Unwrap exposes both the stable kind and an underlying cause.
func (e *Error) Unwrap() []error {
	if e.Err == nil {
		return []error{e.Kind}
	}
	return []error{e.Kind, e.Err}
}

// Annotate adds a downstream stage without changing source location.
func Annotate(err error, stage string) error {
	if err == nil {
		return nil
	}
	var me *Error
	if !errors.As(err, &me) {
		return E(ErrUnlocatedCorrupt, stage, Meta{RecordNo: -1}, err)
	}
	cp := *me
	cp.Stages = append(append([]string(nil), me.Stages...), stage)
	cp.Stage = stage
	return &cp
}

// PrependStage records an already-completed upstream stage without changing
// source location. It is used when parsing succeeded but validation failed.
func PrependStage(err error, stage string) error {
	if err == nil {
		return nil
	}
	me, ok := First(err)
	if !ok {
		return E(ErrUnlocatedCorrupt, stage, Meta{RecordNo: -1}, err)
	}
	cp := *me
	if len(cp.Stages) > 0 && cp.Stages[0] == stage {
		return &cp
	}
	cp.Stages = append([]string{stage}, cp.Stages...)
	return &cp
}

// First extracts a unified error.
func First(err error) (*Error, bool) {
	var me *Error
	ok := errors.As(err, &me)
	return me, ok
}

// Located reports whether the error carries a positive record position.
func Located(err error) bool {
	me, ok := First(err)
	return ok && me.Meta.RecordNo > 0
}

// CodeOf maps a sentinel to a stable machine-readable code.
func CodeOf(kind error) string {
	switch {
	case errors.Is(kind, ErrCRC32):
		return "crc32_mismatch"
	case errors.Is(kind, ErrTimestampOrder):
		return "timestamp_reversed"
	case errors.Is(kind, ErrSequenceOrder):
		return "sequence_reversed"
	case errors.Is(kind, ErrTransitionEndpoint):
		return "missing_transition_endpoint"
	case errors.Is(kind, ErrTruncatedRecord):
		return "truncated_record"
	case errors.Is(kind, ErrMalformedRecord):
		return "malformed_record"
	case errors.Is(kind, ErrBoundaryMismatch):
		return "boundary_mismatch"
	case errors.Is(kind, ErrDurationMismatch):
		return "duration_mismatch"
	case errors.Is(kind, ErrInputFingerprint):
		return "input_fingerprint_mismatch"
	case errors.Is(kind, ErrConfigFingerprint):
		return "config_fingerprint_mismatch"
	case errors.Is(kind, ErrCheckpoint):
		return "checkpoint_invalid"
	case errors.Is(kind, ErrChunkDigest):
		return "chunk_digest_mismatch"
	case errors.Is(kind, ErrInterrupted):
		return "interrupted"
	case errors.Is(kind, ErrArgument):
		return "invalid_argument"
	default:
		return "relay_error"
	}
}

// ExitCode maps an error to one of the terminal's documented exit codes.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	switch {
	case errors.Is(err, ErrArgument):
		return ExitUsage
	case errors.Is(err, ErrInterrupted):
		return ExitRecoverable
	case errors.Is(err, ErrUnlocatedCorrupt):
		return ExitCorruptUnlocated
	default:
		return 1
	}
}
