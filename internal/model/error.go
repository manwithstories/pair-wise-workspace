package model

import "fmt"

// Stage identifiers attached to every pipeline error. They let the CLI map
// failures to exit codes and let the manifest record where damage occurred.
const (
	StageDemux    = "demux"
	StageTrack    = "track"
	StageClock    = "clock"
	StageRTP      = "rtp"
	StagePool     = "pool"
	StageMerge    = "merge"
	StageRemux    = "remux"
	StageCheckpt  = "checkpoint"
	StageManifest = "manifest"
	StageCLI      = "cli"
)

// Error is the unified error type carried through the whole pipeline. It pins
// a failure to a processing stage, a track (TrackID < 0 means not track
// specific) and a byte/sample offset, and states whether the damage may be
// recovered from by dropping or truncating at that point.
type Error struct {
	Stage       string
	TrackID     int32
	Offset      int64  // byte offset inside the input container, or -1
	SampleSeq   uint32 // original sample sequence within the track, or 0
	Recoverable bool
	Reason      string
	Cause       error
}

// NewError builds a non-recoverable stage error.
func NewError(stage, reason string) *Error {
	return &Error{Stage: stage, TrackID: -1, Offset: -1, Reason: reason}
}

// WithTrack annotates the error with a track id and returns it.
func (e *Error) WithTrack(trackID int32) *Error { e.TrackID = trackID; return e }

// WithOffset annotates the error with a byte offset and returns it.
func (e *Error) WithOffset(off int64) *Error { e.Offset = off; return e }

// WithSample annotates the error with an original sample sequence number.
func (e *Error) WithSample(seq uint32) *Error { e.SampleSeq = seq; return e }

// Recover marks the error as recoverable and returns it.
func (e *Error) Recover() *Error { e.Recoverable = true; return e }

// Wrap attaches a cause and returns the error.
func (e *Error) Wrap(cause error) *Error { e.Cause = cause; return e }

func (e *Error) Error() string {
	s := e.Stage + ": " + e.Reason
	if e.TrackID >= 0 {
		s += fmt.Sprintf(" (track=%d", e.TrackID)
		if e.SampleSeq != 0 {
			s += fmt.Sprintf(", sample_seq=%d", e.SampleSeq)
		}
		if e.Offset >= 0 {
			s += fmt.Sprintf(", offset=%d", e.Offset)
		}
		s += ")"
	} else if e.Offset >= 0 {
		s += fmt.Sprintf(" (offset=%d)", e.Offset)
	}
	if e.Recoverable {
		s += " [recoverable]"
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }

// AsModelError extracts a *model.Error from any error chain.
func AsModelError(err error) (*Error, bool) {
	for err != nil {
		if me, ok := err.(*Error); ok {
			return me, true
		}
		if u, ok := err.(interface{ Unwrap() error }); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
	return nil, false
}
