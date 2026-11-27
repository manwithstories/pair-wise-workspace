package media

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrorKind classifies every failure avpipe can report. It is a small closed
// set so the terminal adapter can map a failure to a process exit code and so
// audits can group failures consistently across modules.
type ErrorKind int

const (
	// KindInternal is an avpipe bug or an unrecoverable internal condition.
	KindInternal ErrorKind = iota
	// KindUsage is a malformed command line or configuration.
	KindUsage
	// KindCorrupt is damaged media (truncated KLV/box, bad tables, invalid
	// rotation, out-of-bounds crop) or an unsupported container feature.
	KindCorrupt
	// KindCancel is a cancellation requested by the operator.
	KindCancel
	// KindPolicy is a rejected but well-formed input (e.g. missing or duplicate
	// track mapping, DTS regression that must not be silently reordered).
	KindPolicy
)

// String renders the stable kind token used in manifests and terminals.
func (k ErrorKind) String() string {
	switch k {
	case KindUsage:
		return "usage"
	case KindCorrupt:
		return "corrupt"
	case KindCancel:
		return "cancel"
	case KindPolicy:
		return "policy"
	default:
		return "internal"
	}
}

// Exit codes agreed with the acceptance contract.
const (
	ExitOK       = 0
	ExitInternal = 1
	ExitUsage    = 2
	ExitCorrupt  = 3
	ExitCancel   = 4
)

// ExitCode maps an error kind to the documented process exit code.
func (k ErrorKind) ExitCode() int {
	switch k {
	case KindUsage:
		return ExitUsage
	case KindCorrupt:
		return ExitCorrupt
	case KindCancel:
		return ExitCancel
	default:
		return ExitInternal
	}
}

// Origin points at the exact place in a source file a failure came from.
// Offset is the byte offset inside the file, TrackID and PacketSeq are 0 when
// the failure is not tied to a specific track or packet. The fields are carried
// through every stage so an operator can trace a merge or output decision back
// to the bytes that caused it.
type Origin struct {
	File      string `json:"file,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	TrackID   string `json:"track_id,omitempty"`
	PacketSeq int64  `json:"packet_seq,omitempty"`
	Stage     string `json:"stage,omitempty"`
}

// IsZero reports whether the origin has no location information at all.
func (o Origin) IsZero() bool {
	return o.File == "" && o.Offset == 0 && o.TrackID == "" && o.PacketSeq == 0 && o.Stage == ""
}

// Error is the single error type crossing module boundaries. It always carries
// an ErrorKind and an Origin, and it wraps the underlying cause.
type Error struct {
	Kind   ErrorKind
	Origin Origin
	Op     string
	Msg    string
	Err    error
}

// New builds an Error.
func New(kind ErrorKind, origin Origin, op, msg string, cause error) *Error {
	return &Error{Kind: kind, Origin: origin, Op: op, Msg: msg, Err: cause}
}

// Errorf builds an Error without a cause.
func Errorf(kind ErrorKind, origin Origin, op, format string, args ...any) *Error {
	return New(kind, origin, op, fmt.Sprintf(format, args...), nil)
}

func (e *Error) Error() string {
	s := e.Kind.String() + ": "
	if e.Origin.Stage != "" {
		s += "[" + e.Origin.Stage + "] "
	}
	if e.Op != "" {
		s += e.Op + ": "
	}
	s += e.Msg
	if o := e.Origin; o.File != "" || o.Offset != 0 || o.TrackID != "" || o.PacketSeq != 0 {
		s += " (file=" + o.File
		if o.Offset > 0 {
			s += " offset=" + strconv.FormatInt(o.Offset, 10) +
				"/0x" + strconv.FormatInt(o.Offset, 16)
		}
		if o.TrackID != "" {
			s += " track=" + o.TrackID
		}
		if o.PacketSeq > 0 {
			s += " seq=" + strconv.FormatInt(o.PacketSeq, 10)
		}
		s += ")"
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap exposes the cause for errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.Err }

// WithStage returns a copy of e tagged with a pipeline stage, preserving
// everything else. Modules call this where they observe someone else's error.
func (e *Error) WithStage(stage string) *Error {
	cp := *e
	if cp.Origin.Stage == "" {
		cp.Origin.Stage = stage
	}
	return &cp
}

// WithOrigin returns a copy of e carrying a more specific origin. Nonzero
// fields of the supplied origin override the copy only when set.
func (e *Error) WithOrigin(o Origin) *Error {
	cp := *e
	if o.File != "" {
		cp.Origin.File = o.File
	}
	if o.Offset != 0 {
		cp.Origin.Offset = o.Offset
	}
	if o.TrackID != "" {
		cp.Origin.TrackID = o.TrackID
	}
	if o.PacketSeq != 0 {
		cp.Origin.PacketSeq = o.PacketSeq
	}
	return &cp
}

// KindOf extracts the ErrorKind of any error, defaulting to KindInternal.
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// ExitCodeFor maps any error to the documented exit code.
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}
	return KindOf(err).ExitCode()
}

// AsError unwraps any error into the nearest *Error.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
