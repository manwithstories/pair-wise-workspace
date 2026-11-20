// Package fault defines the error model shared by every pipeline stage.
// Each error carries the stage that raised it, the track it belongs to,
// the raw byte offset in the source buffer, and whether processing may
// continue past it.
package fault

import "fmt"

// Stage identifies the pipeline phase that raised an error.
type Stage string

const (
	StageManifest Stage = "manifest"
	StageSegment  Stage = "segment"
	StageAlign    Stage = "align"
	StageRTP      Stage = "rtp"
	StageVerify   Stage = "verify"
)

// NoOffset marks errors that cannot be tied to a byte position.
const NoOffset = int64(-1)

// Error is a located, classified pipeline fault.
type Error struct {
	Stage       Stage  `json:"stage"`
	Track       string `json:"track"`
	Offset      int64  `json:"source_offset"`
	Recoverable bool   `json:"recoverable"`
	Message     string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s[%s]@%d: %s", e.Stage, e.Track, e.Offset, e.Message)
}

// New builds an Error; offset is NoOffset when not byte-locatable.
func New(stage Stage, track string, offset int64, recoverable bool, format string, args ...interface{}) *Error {
	return &Error{
		Stage:       stage,
		Track:       track,
		Offset:      offset,
		Recoverable: recoverable,
		Message:     fmt.Sprintf(format, args...),
	}
}

// List collects faults. Recoverable faults annotate the output manifest;
// fatal ones terminate their track but never other tracks.
type List []*Error

// Fatal returns the non-recoverable subset.
func (l List) Fatal() List {
	var out List
	for _, e := range l {
		if !e.Recoverable {
			out = append(out, e)
		}
	}
	return out
}

// HasFatal reports whether any fault is non-recoverable.
func (l List) HasFatal() bool { return len(l.Fatal()) > 0 }
