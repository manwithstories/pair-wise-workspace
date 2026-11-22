package meta

import (
	"fmt"
	"sort"

	"mediasim/internal/clock"
)

// Kind identifies a timed-metadata event class.
type Kind int

const (
	Subtitle Kind = iota
	Chapter
	ID3
	SEI
	Emsg
)

func (k Kind) String() string {
	switch k {
	case Subtitle:
		return "subtitle"
	case Chapter:
		return "chapter"
	case ID3:
		return "id3"
	case SEI:
		return "sei"
	case Emsg:
		return "emsg"
	}
	return "unknown"
}

// Event is one timed-metadata item before/after alignment. RawTS is in
// the source track's native clock; MediaTime is the aligned 90 kHz media
// time filled in by Align.
type Event struct {
	Kind      Kind
	Track     string
	RawTS     uint32
	MediaTime int64
	Arrival   int64 // arrival time in media ticks
	Payload   string
	Late      bool // arrived after its media position had passed
}

// Aligner converts metadata events onto the shared media timeline.
type Aligner struct {
	tl *clock.Timeline
}

// NewAligner builds an Aligner over the given timeline.
func NewAligner(tl *clock.Timeline) *Aligner {
	return &Aligner{tl: tl}
}

// Align converts each event's native timestamp to media time and returns
// events sorted by media time. Late events are flagged in place — their
// media position is preserved, never silently shifted. Unknown tracks
// and invalid timestamps produce located errors.
func (a *Aligner) Align(events []Event) ([]Event, error) {
	out := make([]Event, len(events))
	for i, ev := range events {
		mt, err := a.tl.ToMedia(ev.Track, ev.RawTS)
		if err != nil {
			return nil, fmt.Errorf("meta: align %s event %d: %w", ev.Kind, i, err)
		}
		ev.MediaTime = mt
		if ev.Arrival > mt {
			ev.Late = true
		}
		out[i] = ev
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MediaTime != out[j].MediaTime {
			return out[i].MediaTime < out[j].MediaTime
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}

// CheckMonotonic verifies that aligned event times never jump backwards
// relative to the previously committed horizon. A violation is reported
// explicitly rather than silently reordered.
func CheckMonotonic(events []Event, horizon int64) error {
	for i, ev := range events {
		if ev.MediaTime < horizon {
			return fmt.Errorf("meta: %s event %d at media time %d precedes committed horizon %d",
				ev.Kind, i, ev.MediaTime, horizon)
		}
	}
	return nil
}
