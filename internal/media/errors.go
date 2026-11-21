package media

import (
	"errors"
	"fmt"
)

// Class is the stable, machine-readable classification of a media error.
// Classes are reported verbatim on the terminal and are part of the CLI
// contract, so their spelling must not change.
type Class string

const (
	// ClassTruncated means a box, KLV or sample extends past the available
	// input. No bytes are read beyond the reported offset.
	ClassTruncated Class = "truncated"
	// ClassInvalidLength means a declared length is structurally invalid
	// (zero, contradictory, or beyond the enclosing element).
	ClassInvalidLength Class = "invalid_length"
	// ClassMalformed is used for structural problems that are not length
	// related, such as overlapping sample tables.
	ClassMalformed Class = "malformed"
	// ClassUnsupportedCodec means the container parsed but the codec or
	// container version is outside the supported set.
	ClassUnsupportedCodec Class = "unsupported_codec"
	// ClassTimestampAmbiguous means a wrapping timestamp transition cannot
	// be resolved from continuity.
	ClassTimestampAmbiguous Class = "timestamp_ambiguous"
	// ClassDuplicateTrackID means two tracks claim the same stable ID.
	ClassDuplicateTrackID Class = "duplicate_track_id"
	// ClassMissingTrackID means a track has no stable identifier.
	ClassMissingTrackID Class = "missing_track_id"
	// ClassMetadataConflict means geometry or colour descriptions cannot be
	// reconciled, for example a crop outside the coded frame.
	ClassMetadataConflict Class = "metadata_conflict"
	// ClassCheckpointCorrupt means a checkpoint or output segment failed
	// verification and must be rolled back.
	ClassCheckpointCorrupt Class = "checkpoint_corrupt"
	// ClassCancelled means the caller cancelled the operation.
	ClassCancelled Class = "cancelled"
	// ClassIO means an operating-system level read or write failure.
	ClassIO Class = "io"
	// ClassUsage means the command line was invalid.
	ClassUsage Class = "usage"
)

// ExitCode returns the documented process exit code for a class.
// Usage errors are 1, corruption is 2, everything else is 2 because it
// indicates the input could not be trusted. Success is 0.
func (c Class) ExitCode() int {
	if c == ClassUsage {
		return 1
	}
	return 2
}

// Error is the single error type shared by adapters, metadata resolution,
// the pipeline and the muxer. Errors are comparable through errors.Is and
// expose container, track and byte offset for diagnosis.
type Error struct {
	Class     Class
	Msg       string
	Container string // "mp4", "mxf" or empty
	Track     uint32 // stable track ID; 0 means not track specific
	HasTrack  bool
	Offset    int64 // byte offset in the input, -1 when unknown
	Tolerance int64 // bytes that may legitimately be missing
	Cause     error
}

// NewError creates an error with an unknown input offset.
func NewError(class Class, format string, args ...any) *Error {
	return &Error{Class: class, Msg: fmt.Sprintf(format, args...), Offset: -1}
}

func (e *Error) Error() string {
	msg := e.Msg
	if e.Container != "" {
		msg = e.Container + ": " + msg
	}
	if e.HasTrack {
		msg = fmt.Sprintf("%s (track %d)", msg, e.Track)
	}
	if e.Offset >= 0 {
		msg = fmt.Sprintf("%s at input offset %d", msg, e.Offset)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

// Is matches on class so callers can write errors.Is(err, media.ErrTruncated)
// without depending on the message.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Class == e.Class
}

// WithContainer records the container type that produced the error.
func (e *Error) WithContainer(c string) *Error { e.Container = c; return e }

// WithTrack records the stable track identifier.
func (e *Error) WithTrack(id uint32) *Error { e.Track, e.HasTrack = id, true; return e }

// WithOffset records the exact byte offset where the problem was detected.
func (e *Error) WithOffset(off int64) *Error { e.Offset = off; return e }

// WithTolerance records how many trailing bytes may be absent without
// changing the classification. Strict input uses zero.
func (e *Error) WithTolerance(n int64) *Error { e.Tolerance = n; return e }

// WithCause attaches an underlying error while preserving the class.
func (e *Error) WithCause(err error) *Error { e.Cause = err; return e }

// Sentinel values used with errors.Is to match a class.
var (
	ErrTruncated          = &Error{Class: ClassTruncated}
	ErrInvalidLength      = &Error{Class: ClassInvalidLength}
	ErrMalformed          = &Error{Class: ClassMalformed}
	ErrUnsupportedCodec   = &Error{Class: ClassUnsupportedCodec}
	ErrTimestampAmbiguous = &Error{Class: ClassTimestampAmbiguous}
	ErrDuplicateTrackID   = &Error{Class: ClassDuplicateTrackID}
	ErrMissingTrackID     = &Error{Class: ClassMissingTrackID}
	ErrMetadataConflict   = &Error{Class: ClassMetadataConflict}
	ErrCheckpointCorrupt  = &Error{Class: ClassCheckpointCorrupt}
	ErrCancelled          = &Error{Class: ClassCancelled}
	ErrIO                 = &Error{Class: ClassIO}
	ErrUsage              = &Error{Class: ClassUsage}
)

// ClassOf extracts the classification of err, defaulting to ClassMalformed
// for foreign errors.
func ClassOf(err error) Class {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ClassMalformed
}

// OffsetOf returns the byte offset carried by err, or -1.
func OffsetOf(err error) int64 {
	var e *Error
	if errors.As(err, &e) {
		return e.Offset
	}
	return -1
}

// ExitCodeFor maps an error to the documented process exit code.
func ExitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	return ClassOf(err).ExitCode()
}
