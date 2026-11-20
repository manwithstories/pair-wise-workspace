// Package model defines the cross-module data and error types shared by
// every pipeline stage. Errors carry a machine-readable Kind and a
// human-locatable Location so the batch runner can classify a failure and
// the auditor can pinpoint the packet, track or block it came from.
package model

import "fmt"

// Kind classifies a pipeline failure by stage. The command layer maps each
// Kind onto a process exit code.
type Kind string

const (
	KindPacket   Kind = "packet"   // malformed RTP/RTCP datagram
	KindClock    Kind = "clock"    // timebase conversion, wrap or drift
	KindRepair   Kind = "repair"   // reorder/dedup/loss recovery
	KindTimeline Kind = "timeline" // track mixing and boundary logic
	KindRender   Kind = "render"   // frame/segment export
	KindIndex    Kind = "index"    // chunked index and checkpoint I/O
	KindInput    Kind = "input"    // input/parameter digest mismatch
	KindCanceled Kind = "canceled" // context cancellation
	KindIO       Kind = "io"       // filesystem write/read failure
	KindParam    Kind = "param"    // invalid command parameters
)

// Error is the structured error used across modules. Location pinpoints
// the failing object (e.g. "ssrc=0x1 seq=12", "chunk 3", "track video").
type Error struct {
	Kind     Kind
	Location string
	Message  string
	Cause    error
}

// Error implements the error interface.
func (e *Error) Error() string {
	s := string(e.Kind)
	if e.Location != "" {
		s += " at " + e.Location
	}
	s += ": " + e.Message
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }

// New builds an *Error. cause may be nil.
func New(kind Kind, location, message string, cause error) *Error {
	return &Error{Kind: kind, Location: location, Message: message, Cause: cause}
}

// Newf is New with a formatted message.
func Newf(kind Kind, location, format string, args ...interface{}) *Error {
	return &Error{Kind: kind, Location: location, Message: fmt.Sprintf(format, args...)}
}

// AsError extracts a *Model error from err, returning nil if it is another
// error type.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	if me, ok := err.(*Error); ok {
		return me
	}
	return nil
}
