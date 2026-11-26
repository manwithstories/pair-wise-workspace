package media

import (
	"errors"
	"fmt"
)

// FaultKind classifies a failure against the source material. The kind is
// stable across subsystems so a demuxer can report a truncation and the
// pipeline can quarantine it using the same vocabulary.
type FaultKind int

const (
	FaultUnknown FaultKind = iota
	FaultManifestSyntax
	FaultManifestRange // out-of-bounds URI, duplicate sequence, bad duration
	FaultTruncated     // short read: fewer bytes than the container requires
	FaultBadBoxLength  // fMP4 box size exceeds parent or is negative
	FaultContinuity    // TS continuity counter break
	FaultMissingInit   // media segment without its initialisation section
	FaultTimestampWraparound
	FaultDurationMismatch
	FaultUnsupportedCodec
	FaultStateInvariant
	FaultCancelled
	FaultIO
)

var faultNames = map[FaultKind]string{
	FaultUnknown:             "unknown",
	FaultManifestSyntax:      "manifest-syntax",
	FaultManifestRange:       "manifest-range",
	FaultTruncated:           "truncated",
	FaultBadBoxLength:        "bad-box-length",
	FaultContinuity:          "continuity",
	FaultMissingInit:         "missing-init",
	FaultTimestampWraparound: "timestamp-wraparound",
	FaultDurationMismatch:    "duration-mismatch",
	FaultUnsupportedCodec:    "unsupported-codec",
	FaultStateInvariant:      "state-invariant",
	FaultCancelled:           "cancelled",
	FaultIO:                  "io",
}

// String renders the kind in the JSON/CLI vocabulary.
func (k FaultKind) String() string {
	if s, ok := faultNames[k]; ok {
		return s
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Recoverable reports whether processing can continue past this fault. Truncated
// segments, bad box lengths and continuity breaks are isolated and the remaining
// segments still process; state invariant failures are not.
func (k FaultKind) Recoverable() bool {
	switch k {
	case FaultTruncated, FaultBadBoxLength, FaultContinuity, FaultMissingInit,
		FaultDurationMismatch, FaultManifestRange:
		return true
	}
	return false
}

// SourcePos is a position inside an input artifact. Byte offsets are absolute
// within the file the reporter read (a segment file or a manifest). Line is
// 1-based and zero when the artifact has no lines (binary segments).
type SourcePos struct {
	Artifact string // file name or logical name, never an absolute path
	Line     int
	Offset   int64 // absolute byte offset; -1 when unknown
	Segment  int   // manifest segment index; -1 when not a segment
}

// IsZero reports whether no position was supplied.
func (p SourcePos) IsZero() bool {
	return p.Artifact == "" && p.Line == 0 && p.Offset < 0 && p.Segment < 0
}

// String renders "artifact:line+offset@segment" with empty parts omitted.
func (p SourcePos) String() string {
	s := p.Artifact
	if s == "" {
		s = "<source>"
	}
	if p.Line > 0 {
		s += fmt.Sprintf(":%d", p.Line)
	}
	if p.Offset >= 0 {
		s += fmt.Sprintf("+%d", p.Offset)
	}
	if p.Segment >= 0 {
		s += fmt.Sprintf("@seg%d", p.Segment)
	}
	return s
}

// Fault is the shared error type. Every parser, demuxer, muxer and verifier
// wraps its failures in a *Fault so the CLI can print a stable machine-readable
// line, and the pipeline can decide quarantine vs abort purely from Kind.
type Fault struct {
	Kind FaultKind
	Pos  SourcePos
	Msg  string
	Err  error
}

// NewFault builds a fault at a known position.
func NewFault(kind FaultKind, pos SourcePos, msg string) *Fault {
	return &Fault{Kind: kind, Pos: pos, Msg: msg}
}

// WrapFault attaches a kind and position to an underlying error.
func WrapFault(kind FaultKind, pos SourcePos, err error) *Fault {
	if f, ok := AsFault(err); ok {
		return f
	}
	return &Fault{Kind: kind, Pos: pos, Msg: err.Error(), Err: err}
}

// At returns a copy of the fault after filling in any unset position field.
// Parsers use it to enrich a fault with the line and offset they discovered
// without rewriting the message.
func (f *Fault) At(pos SourcePos) *Fault {
	cp := *f
	if cp.Pos.Artifact == "" {
		cp.Pos.Artifact = pos.Artifact
	}
	if cp.Pos.Line == 0 {
		cp.Pos.Line = pos.Line
	}
	if cp.Pos.Offset < 0 {
		cp.Pos.Offset = pos.Offset
	}
	if cp.Pos.Segment < 0 {
		cp.Pos.Segment = pos.Segment
	}
	return &cp
}

// Error implements error.
func (f *Fault) Error() string {
	msg := f.Msg
	if msg == "" && f.Err != nil {
		msg = f.Err.Error()
	}
	return fmt.Sprintf("%s: %s: %s", f.Kind, f.Pos, msg)
}

// Unwrap exposes the underlying error to errors.Is/As.
func (f *Fault) Unwrap() error { return f.Err }

// Recoverable reports the quarantine decision for this fault.
func (f *Fault) Recoverable() bool { return f.Kind.Recoverable() }

// AsFault extracts a *Fault from an error chain.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// KindOf reports the fault kind of err, defaulting to FaultUnknown.
func KindOf(err error) FaultKind {
	if f, ok := AsFault(err); ok {
		return f.Kind
	}
	return FaultUnknown
}

// ExitCode maps an error onto the demo's documented exit codes: 2 for bad
// arguments, 3 for unrecoverable media errors, 4 for I/O or invariant failure.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if f, ok := AsFault(err); ok {
		if f.Kind.Recoverable() {
			return 3
		}
		switch f.Kind {
		case FaultStateInvariant, FaultIO, FaultCancelled:
			return 4
		}
		switch f.Kind {
		case FaultUnsupportedCodec, FaultTimestampWraparound:
			return 3
		}
	}
	return 3
}
