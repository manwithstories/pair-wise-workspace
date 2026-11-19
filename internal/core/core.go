// Package core holds the media time base and the shared error model used by
// every mediamix module. Errors always carry a stable code, the track they
// belong to (when known) and, when applicable, the packet sequence number.
package core

import (
	"errors"
	"fmt"
)

// Error codes are stable identifiers that appear in CLI output and JSON.
const (
	CodeBadArgument   = "bad_argument"
	CodeOverLimit     = "over_limit"
	CodeBadIndex      = "bad_index"
	CodeBadTimestamp  = "bad_timestamp"
	CodeBadPacket     = "bad_packet"
	CodeBadGraph      = "bad_graph"
	CodeMissingDep    = "missing_dependency"
	CodeCycle         = "cycle"
	CodeOverlap       = "overlap"
	CodeAudioInvalid  = "audio_invalid"
	CodeSilentAudio   = "audio_silent"
	CodeClippedAudio  = "audio_clipped"
	CodeEmptyTrack    = "empty_track"
	CodeUnrecoverable = "unrecoverable_gap"
	CodeTimeout       = "timeout"
	CodeInternal      = "internal"
)

// Error is the single error type shared across mediamix. It is JSON friendly
// so pipeline reports can embed validation failures without loss of context.
type Error struct {
	Code   string `json:"code"`
	Track  string `json:"track,omitempty"`
	Seq    *int   `json:"seq,omitempty"`
	Detail string `json:"detail"`
	// Fatal marks validation failures that reject the whole input; recoverable
	// per-track failures leave Fatal false so other tracks keep running.
	Fatal bool `json:"fatal"`
}

func (e *Error) Error() string {
	msg := e.Code
	if e.Track != "" {
		msg += " track=" + e.Track
	}
	if e.Seq != nil {
		msg += fmt.Sprintf(" seq=%d", *e.Seq)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// TrackedError returns an Error with Track set, preserving an incoming code
// when err already is a *Error and its code is more specific than fallback.
func TrackedError(code, track, detail string) *Error {
	return &Error{Code: code, Track: track, Detail: detail}
}

// SeqError returns an Error that also records a packet sequence number.
func SeqError(code, track string, seq uint32, detail string) *Error {
	s := int(seq)
	return &Error{Code: code, Track: track, Seq: &s, Detail: detail}
}

// As extracts an *Error from err, returning ok=false for foreign errors.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// CodeOf returns the mediamix code carried by err, or CodeInternal.
func CodeOf(err error) string {
	if e, ok := As(err); ok {
		return e.Code
	}
	return CodeInternal
}

// StampTrack fills the track field of an *Error so callers can attribute a
// failure without losing the original code or sequence number.
func StampTrack(err error, track string) error {
	if e, ok := As(err); ok {
		c := *e
		if c.Track == "" {
			c.Track = track
		}
		return &c
	}
	return &Error{Code: CodeInternal, Track: track, Detail: err.Error()}
}
