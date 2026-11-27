// Package timeline maps the editorial plan of the four-track sample — clip
// in/out points and cross transitions — onto the shared 90 kHz timebase and
// onto the actual record positions of a segment.
//
// Clip boundaries are allowed to fall anywhere in the editorial grid. When a
// boundary lands strictly inside a record, Resolve reports a Boundary fault at
// that record's original byte offset and widens the clip to whole records, so
// isolation downstream never splits a record.
package timeline

import "relayaudit/internal/wire"

// Clip is one programme segment on a single track. Start and End are shared
// 90 kHz ticks measured from the start of the segment.
type Clip struct {
	Track uint8
	Label string
	Start uint64
	End   uint64

	// StartAdjusted and EndAdjusted report that the boundary did not align
	// with a record edge and was widened to cover the containing record.
	StartAdjusted bool
	EndAdjusted   bool
}

// Duration returns the clip length in ticks.
func (c Clip) Duration() uint64 {
	if c.End <= c.Start {
		return 0
	}
	return c.End - c.Start
}

// Transition is a cross fade or cut straddling two tracks.
type Transition struct {
	From  uint8
	To    uint8
	Kind  string
	Label string
	Start uint64
	End   uint64

	StartAdjusted bool
	EndAdjusted   bool
}

// Duration returns the transition length in ticks.
func (t Transition) Duration() uint64 {
	if t.End <= t.Start {
		return 0
	}
	return t.End - t.Start
}

// Plan is the untimed editorial intent.
type Plan struct {
	Clips       []Clip
	Transitions []Transition
}

// Entry is one record placed on the shared timebase.
type Entry struct {
	Rec   *wire.Record
	Start uint64
	End   uint64
}

// Track is a record timeline for one track.
type Track struct {
	ID      uint8
	Entries []Entry
	First   uint64
	Last    uint64
}

// Resolution is a plan bound to concrete records.
type Resolution struct {
	Tracks      map[uint8]*Track
	Clips       []Clip
	Transitions []Transition
	Duration    uint64
}

// defaultFrameTicks is used to close the final record of a track when no
// successor is available: 40 ms video, 21.3 ms audio.
func defaultFrameTicks(track uint8) uint64 {
	if track <= 2 {
		return 3600
	}
	return 1920
}
