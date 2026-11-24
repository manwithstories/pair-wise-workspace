// Package model defines the cross-track event vocabulary shared by the
// transport, parse/index and composition subsystems, plus the error
// taxonomy used to decide whether a failure is local or fatal.
package model

import (
	"fmt"
	"sort"

	"mediarun/internal/clock"
)

// Kind enumerates the timed metadata payloads carried alongside the A/V
// tracks.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindSubtitle
	KindChapter
	KindID3
	KindEmsg
	KindSEI
	KindFrame
)

func (k Kind) String() string {
	switch k {
	case KindSubtitle:
		return "subtitle"
	case KindChapter:
		return "chapter"
	case KindID3:
		return "id3"
	case KindEmsg:
		return "emsg"
	case KindSEI:
		return "sei"
	case KindFrame:
		return "frame"
	default:
		return "unknown"
	}
}

// ParseKind is the inverse of String; it rejects unknown names.
func ParseKind(s string) (Kind, error) {
	switch s {
	case "subtitle":
		return KindSubtitle, nil
	case "chapter":
		return KindChapter, nil
	case "id3":
		return KindID3, nil
	case "emsg":
		return KindEmsg, nil
	case "sei":
		return KindSEI, nil
	case "frame":
		return KindFrame, nil
	default:
		return KindUnknown, fmt.Errorf("%w: kind %q", ErrInvalid, s)
	}
}

// Track identifies the capture source. IDs are stable for the whole run so
// that merged timelines stay reproducible.
type Track struct {
	ID        uint32 `json:"id"`
	Timescale uint32 `json:"timescale"`
	Name      string `json:"name"`
}

// Event is a single timed item after conversion to the unified tick base.
// TrackTime keeps the original track timestamp for diagnostics even though
// ordering always uses Start.
type Event struct {
	Kind         Kind       `json:"kind"`
	TrackID      uint32     `json:"track_id"`
	TrackTime    uint32     `json:"track_time"`
	Start        clock.Tick `json:"start"`
	Duration     clock.Tick `json:"duration"`
	Payload      []byte     `json:"payload,omitempty"`
	PayloadID    string     `json:"payload_id,omitempty"`
	Sequence     uint32     `json:"sequence"`
	Keyframe     bool       `json:"keyframe,omitempty"`
	UnknownBoxes int        `json:"unknown_boxes,omitempty"`
}

// End returns the exclusive end tick.
func (e Event) End() (clock.Tick, error) {
	return clock.Add(e.Start, e.Duration)
}

// Digest is the deterministic byte form used for round-trip comparison and
// hashing. It deliberately excludes nothing: every field that is written
// back must survive a serialize/parse cycle.
func (e Event) Digest() string {
	return fmt.Sprintf("%d|%d|%d|%d|%d|%s|%x|%d|%t|%d",
		e.Kind, e.TrackID, e.TrackTime, e.Start, e.Duration,
		e.PayloadID, e.Payload, e.Sequence, e.Keyframe, e.UnknownBoxes)
}

// Less orders events by start tick, then by kind, track and sequence so
// that simultaneous events merge deterministically across tracks.
func Less(a, b Event) bool {
	if a.Start != b.Start {
		return a.Start < b.Start
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.TrackID != b.TrackID {
		return a.TrackID < b.TrackID
	}
	if a.Sequence != b.Sequence {
		return a.Sequence < b.Sequence
	}
	return a.PayloadID < b.PayloadID
}

// SortStable sorts in place using Less; ties keep insertion order.
func SortStable(events []Event) {
	sort.SliceStable(events, func(i, j int) bool { return Less(events[i], events[j]) })
}

// Gap records a missing span detected by the reorder buffer. It never
// carries fabricated media; consumers use it to annotate verification
// summaries.
type Gap struct {
	TrackID uint32     `json:"track_id"`
	From    uint16     `json:"from"`
	To      uint16     `json:"to"`
	Count   int        `json:"count"`
	Start   clock.Tick `json:"start"`
	End     clock.Tick `json:"end"`
}
