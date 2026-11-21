// Package model defines the shared media clock, track graph and error
// model used by every mediaroute subsystem.
package model

import "fmt"

// Exit codes used by the mediaroute CLI.
const (
	ExitOK            = 0
	ExitCorrupt       = 2 // input or manifest corrupt
	ExitUnrecoverable = 3 // unrecoverable packet gap
	ExitCheckpoint    = 4 // checkpoint mismatch
	ExitInternal      = 5
)

// ErrorCode classifies a MediaError.
type ErrorCode string

const (
	ErrManifest   ErrorCode = "MANIFEST_INVALID"
	ErrDuplicate  ErrorCode = "DUPLICATE_CONFLICT"
	ErrGap        ErrorCode = "UNRECOVERABLE_GAP"
	ErrCorrupt    ErrorCode = "SEGMENT_CORRUPT"
	ErrSync       ErrorCode = "SYNC_DRIFT"
	ErrGraph      ErrorCode = "TRACK_GRAPH_INVALID"
	ErrCheckpoint ErrorCode = "CHECKPOINT_MISMATCH"
	ErrInternal   ErrorCode = "INTERNAL"
)

// MediaError is the shared error context propagated across modules.
type MediaError struct {
	Code      ErrorCode
	Module    string // originating module, e.g. "recovery"
	Track     string // track id, empty if not track-scoped
	Seq       int64  // extended sequence number, -1 if n/a
	Timestamp int64  // media-clock units, -1 if n/a
	Location  string // manifest line or XML path, empty if n/a
	Msg       string
	Cause     error
}

func (e *MediaError) Error() string {
	s := fmt.Sprintf("[%s] %s: %s", e.Code, e.Module, e.Msg)
	if e.Track != "" {
		s += fmt.Sprintf(" track=%s", e.Track)
	}
	if e.Seq >= 0 {
		s += fmt.Sprintf(" seq=%d", e.Seq)
	}
	if e.Timestamp >= 0 {
		s += fmt.Sprintf(" ts=%d", e.Timestamp)
	}
	if e.Location != "" {
		s += fmt.Sprintf(" at=%s", e.Location)
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

func (e *MediaError) Unwrap() error { return e.Cause }

// With returns a copy of the error annotated by another module as it
// propagates, preserving the original context.
func (e *MediaError) With(module, msg string) *MediaError {
	c := *e
	c.Msg = fmt.Sprintf("%s (via %s: %s)", e.Msg, module, msg)
	return &c
}

// ExitCode maps an error to a CLI exit code.
func ExitCode(err error) int {
	me, ok := err.(*MediaError)
	if !ok {
		return ExitInternal
	}
	switch me.Code {
	case ErrManifest, ErrCorrupt, ErrDuplicate, ErrGraph:
		return ExitCorrupt
	case ErrGap:
		return ExitUnrecoverable
	case ErrCheckpoint:
		return ExitCheckpoint
	default:
		return ExitInternal
	}
}

// Clock is a deterministic media clock: integer units at a fixed timescale.
type Clock struct{ Timescale uint32 }

// ToMillis converts clock units to milliseconds.
func (c Clock) ToMillis(units int64) int64 {
	return units * 1000 / int64(c.Timescale)
}

// FromMillis converts milliseconds to clock units.
func (c Clock) FromMillis(ms int64) int64 {
	return ms * int64(c.Timescale) / 1000
}

// TrackKind identifies the essence type of a track.
type TrackKind string

const (
	TrackAudio TrackKind = "audio"
	TrackVideo TrackKind = "video"
)

// Track is one essence lane in the timeline.
type Track struct {
	ID    string
	Kind  TrackKind
	Clock Clock
}

// Segment is a manifest-declared media segment on the timeline.
type Segment struct {
	Index    int
	Track    string
	Duration int64 // clock units of the track
	Start    int64 // clock units of the track
	URI      string
}

// Node is a processing stage in the explicit track graph.
type Node struct {
	ID    string
	Deps  []string
	Stage string // pipeline stage name, e.g. "manifest", "recovery"
}

// Graph is an explicit dependency graph of processing nodes.
type Graph struct{ Nodes []Node }

// TopoSort returns node IDs in dependency order. A missing dependency or
// a cycle yields an ErrGraph MediaError naming the offending node.
func (g *Graph) TopoSort() ([]string, error) {
	known := map[string]Node{}
	for _, n := range g.Nodes {
		known[n.ID] = n
	}
	for _, n := range g.Nodes {
		for _, d := range n.Deps {
			if _, ok := known[d]; !ok {
				return nil, &MediaError{Code: ErrGraph, Module: "model",
					Msg: fmt.Sprintf("node %q depends on missing node %q", n.ID, d)}
			}
		}
	}
	const (white, gray, black = 0, 1, 2)
	color := map[string]int{}
	var order []string
	var visit func(id string) error
	visit = func(id string) error {
		switch color[id] {
		case gray:
			return &MediaError{Code: ErrGraph, Module: "model",
				Msg: fmt.Sprintf("dependency cycle at node %q", id)}
		case black:
			return nil
		}
		color[id] = gray
		for _, d := range known[id].Deps {
			if err := visit(d); err != nil {
				return err
			}
		}
		color[id] = black
		order = append(order, id)
		return nil
	}
	for _, n := range g.Nodes {
		if err := visit(n.ID); err != nil {
			return nil, err
		}
	}
	return order, nil
}
