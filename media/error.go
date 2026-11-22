package media

import "fmt"

// ErrorKind classifies every diagnosable media failure so callers can map
// failures to process exit codes and retry decisions.
type ErrorKind string

const (
	// KindTruncatedBox: an MP4 box header or payload ends past EOF.
	KindTruncatedBox ErrorKind = "truncated-box"
	// KindBadSampleTable: sample table entries are missing, inconsistent or
	// point outside the mdat payload.
	KindBadSampleTable ErrorKind = "bad-sample-table"
	// KindMissingTrack: a track referenced by the manifest/box layout is
	// absent or lacks a required sub-box.
	KindMissingTrack ErrorKind = "missing-track"
	// KindEditList: negative-start, empty or otherwise invalid edit list.
	KindEditList ErrorKind = "edit-list"
	// KindTimestampWrap: a backward RTP timestamp that cannot be proven to
	// be a 32-bit wrap.
	KindTimestampWrap ErrorKind = "timestamp-wrap"
	// KindCheckpoint: resume validation failed (hash/boundary mismatch).
	KindCheckpoint ErrorKind = "checkpoint"
	// KindUnsliceable: no keyframe boundary exists inside the wait window.
	KindUnsliceable ErrorKind = "unsliceable"
	// KindIO: underlying I/O failure.
	KindIO ErrorKind = "io"
)

// MediaError carries enough localization information for an on-call
// engineer to find the offending material without rescanning it: track id,
// byte offset in the source file and the media time interval involved.
type MediaError struct {
	Kind    ErrorKind `json:"kind"`
	Source  string    `json:"source"`
	TrackID uint32    `json:"track_id,omitempty"`
	// ByteOffset is the located byte position; -1 means unknown.
	ByteOffset int64 `json:"byte_offset,omitempty"`
	// ErrorBytes is the scanner's localization uncertainty in bytes; 0 means
	// the offset is exact.
	ErrorBytes int    `json:"error_bytes,omitempty"`
	FromTick   Ticks  `json:"from_tick,omitempty"`
	ToTick     Ticks  `json:"to_tick,omitempty"`
	Message    string `json:"message"`
}

// Error implements the error interface.
func (e *MediaError) Error() string {
	s := string(e.Kind)
	if e.Source != "" {
		s += " " + e.Source
	}
	if e.TrackID != 0 {
		s += fmt.Sprintf(" track=%d", e.TrackID)
	}
	if e.ByteOffset >= 0 {
		s += fmt.Sprintf(" offset=%d±%dB", e.ByteOffset, e.ErrorBytes)
	}
	if e.ToTick > e.FromTick {
		s += fmt.Sprintf(" ticks=[%d,%d)", e.FromTick, e.ToTick)
	}
	return s + ": " + e.Message
}

// NewError builds a MediaError with the given kind and message.
func NewError(kind ErrorKind, source string, msg string) *MediaError {
	return &MediaError{Kind: kind, Source: source, ByteOffset: -1, Message: msg}
}

// WithTrack attaches a track id and returns the receiver for chaining.
func (e *MediaError) WithTrack(id uint32) *MediaError {
	e.TrackID = id
	return e
}

// WithOffset attaches an exact or approximate byte offset. errorBytes is the
// localization uncertainty and is capped at MaxLocateErrorBytes.
func (e *MediaError) WithOffset(offset int64, errorBytes int) *MediaError {
	if errorBytes > MaxLocateErrorBytes {
		errorBytes = MaxLocateErrorBytes
	}
	if errorBytes < 0 {
		errorBytes = 0
	}
	e.ByteOffset = offset
	e.ErrorBytes = errorBytes
	return e
}

// WithInterval attaches the affected media-time interval.
func (e *MediaError) WithInterval(from, to Ticks) *MediaError {
	e.FromTick, e.ToTick = from, to
	return e
}

// MaxLocateErrorBytes is the contractual upper bound on localization
// uncertainty (acceptance: locate_error_bytes<=16).
const MaxLocateErrorBytes = 16

// ErrorList collects per-track errors so one bad track never discards
// validated results from the others.
type ErrorList []*MediaError

// Error implements the error interface.
func (el ErrorList) Error() string {
	switch len(el) {
	case 0:
		return ""
	case 1:
		return el[0].Error()
	default:
		return fmt.Sprintf("%d media errors (first: %s)", len(el), el[0].Error())
	}
}

// First returns the earliest error of the requested kind, or nil.
func (el ErrorList) First(kind ErrorKind) *MediaError {
	for _, e := range el {
		if e.Kind == kind {
			return e
		}
	}
	return nil
}
