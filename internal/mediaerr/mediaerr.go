// Package mediaerr defines the shared media error type used by the
// bitstream scan, clock and cache/recovery subsystems. Every error
// carries the track, byte offset and timestamp it relates to, plus a
// recoverability flag telling the pipeline whether the affected track
// can be isolated while the remaining tracks continue.
package mediaerr

import (
	"errors"
	"fmt"
)

// Subsystem identifies where an error originated.
type Subsystem string

const (
	Scan    Subsystem = "scan"
	Clock   Subsystem = "clock"
	Cache   Subsystem = "cache"
	Compose Subsystem = "compose"
	Render  Subsystem = "render"
)

// NA is used for Offset/Timestamp fields that do not apply.
const NA = -1

// Error is the common media error. Offset is a byte offset in the
// source stream, Timestamp a stream timestamp; both are NA when not
// applicable. Recoverable reports whether the pipeline may isolate the
// affected track and keep processing the others.
type Error struct {
	Subsystem   Subsystem
	Track       string
	Offset      int64
	Timestamp   int64
	Recoverable bool
	Msg         string
}

// New builds an *Error with a formatted message.
func New(sub Subsystem, track string, offset, ts int64, recoverable bool, format string, args ...any) *Error {
	return &Error{
		Subsystem:   sub,
		Track:       track,
		Offset:      offset,
		Timestamp:   ts,
		Recoverable: recoverable,
		Msg:         fmt.Sprintf(format, args...),
	}
}

func (e *Error) Error() string {
	s := string(e.Subsystem) + ": " + e.Msg
	if e.Track != "" {
		s += " track=" + e.Track
	}
	if e.Offset != NA {
		s += fmt.Sprintf(" offset=%d", e.Offset)
	}
	if e.Timestamp != NA {
		s += fmt.Sprintf(" ts=%d", e.Timestamp)
	}
	if e.Recoverable {
		s += " (recoverable)"
	} else {
		s += " (fatal)"
	}
	return s
}

// As extracts an *Error from err's chain.
func As(err error) (*Error, bool) {
	var me *Error
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// Recoverable reports whether err is a recoverable media error.
func Recoverable(err error) bool {
	if me, ok := As(err); ok {
		return me.Recoverable
	}
	return false
}
