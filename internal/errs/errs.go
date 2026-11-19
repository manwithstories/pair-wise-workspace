// Package errs defines the unified, locatable error type shared by every
// pipeline stage (manifest, segment, clock, packet, demux, mapping, remux,
// index, io). Batch processing uses the Kind to keep going on other assets
// and the Location to pinpoint the failing segment/box/track.
package errs

import "fmt"

// Kind classifies pipeline errors by stage.
type Kind string

const (
	KindManifest Kind = "manifest" // playlist/MPD parsing and validation
	KindSegment  Kind = "segment"  // segment fetch, dedup, ordering, gaps
	KindClock    Kind = "clock"    // timebase conversion and wrap recovery
	KindPacket   Kind = "packet"   // RTP packet recovery
	KindDemux    Kind = "demux"    // container demuxing
	KindMapping  Kind = "mapping"  // track mapping
	KindRemux    Kind = "remux"    // lossless remuxing
	KindIndex    Kind = "index"    // checkpoint index
	KindIO       Kind = "io"       // filesystem reads/writes
)

// Error is a locatable pipeline error. Asset names the asset being
// processed; Location pinpoints the failure (e.g. "segment 7",
// "box offset 4096", "track 2").
type Error struct {
	Kind     Kind
	Asset    string
	Location string
	Msg      string
	Err      error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: asset %q", e.Kind, e.Asset)
	if e.Location != "" {
		s += " at " + e.Location
	}
	s += ": " + e.Msg
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an *Error. err may be nil.
func New(kind Kind, asset, location, msg string, err error) *Error {
	return &Error{Kind: kind, Asset: asset, Location: location, Msg: msg, Err: err}
}

// Newf is New with a formatted message.
func Newf(kind Kind, asset, location, format string, args ...interface{}) *Error {
	return &Error{Kind: kind, Asset: asset, Location: location, Msg: fmt.Sprintf(format, args...)}
}
