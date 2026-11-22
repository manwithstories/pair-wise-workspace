// Package timeline merges subtitle cues and timecode markers onto the
// unified nanosecond time base and records deterministic rulings for
// rate conflicts and chunk-spanning cues.
package timeline

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"

	"mediasync/internal/clock"
	"mediasync/internal/defect"
	"mediasync/internal/subtitle"
)

// Event is one merged timeline entry.
type Event struct {
	Track string
	Start int64
	End   int64 // == Start for instant markers
	Label string
}

// Marker is a timecode marker sample.
type Marker struct {
	Rate  clock.Rate
	Nanos int64
	Label string
}

// Decision records a deterministic ruling made during the merge.
type Decision struct {
	Kind   string `json:"kind"`
	At     int64  `json:"at_nanos"`
	Detail string `json:"detail"`
}

// Merger accumulates events chunk by chunk. Memory stays bounded: only
// a running hash and the ruling log are kept.
type Merger struct {
	canonical clock.Rate
	rateSet   bool
	h         hash.Hash
	// Decisions lists every ruling in the order it was made.
	Decisions []Decision
	// Defects mirrors the rulings into the shared error model.
	Defects []defect.Defect
}

// NewMerger returns an empty Merger.
func NewMerger() *Merger { return &Merger{h: sha256.New()} }

// AddChunk merges one chunk of samples and returns its events.
// chunkEnd is the chunk's time horizon; cues still active past it get a
// deterministic span-chunk ruling and are carried over by the caller.
func (m *Merger) AddChunk(cues []subtitle.Cue, markers []Marker, chunkEnd int64) []Event {
	var evs []Event
	for _, c := range cues {
		ev := Event{Track: "subtitle", Start: c.Start, End: c.End, Label: c.Text}
		m.writeEvent(ev)
		if c.End > chunkEnd {
			d := Decision{Kind: "span_chunk", At: c.Start,
				Detail: fmt.Sprintf("cue %d [%d,%d) crosses chunk horizon %d; assigned to chunk containing start",
					c.Index, c.Start, c.End, chunkEnd)}
			m.Decisions = append(m.Decisions, d)
			m.Defects = append(m.Defects, defect.New("subtitle", -1, c.Offset, defect.SpanChunk, d.Detail))
		}
		evs = append(evs, ev)
	}
	for _, mk := range markers {
		if !m.rateSet {
			m.canonical, m.rateSet = mk.Rate, true
		} else if mk.Rate != m.canonical {
			d := Decision{Kind: "rate_conflict", At: mk.Nanos,
				Detail: fmt.Sprintf("marker rate %s conflicts with canonical %s; marker kept on its own rate",
					mk.Rate, m.canonical)}
			m.Decisions = append(m.Decisions, d)
			m.Defects = append(m.Defects, defect.New("timecode", -1, -1, defect.TimecodeConflict, d.Detail))
		}
		ev := Event{Track: "timecode", Start: mk.Nanos, End: mk.Nanos, Label: mk.Label}
		m.writeEvent(ev)
		evs = append(evs, ev)
	}
	return evs
}

func (m *Merger) writeEvent(ev Event) {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(ev.Start))
	binary.BigEndian.PutUint64(buf[8:16], uint64(ev.End))
	m.h.Write([]byte(ev.Track))
	m.h.Write([]byte{0})
	m.h.Write(buf[:])
	m.h.Write([]byte(ev.Label))
	m.h.Write([]byte{0})
}

// Summary returns the hex digest of every merged event in order; equal
// summaries prove identical timelines.
func (m *Merger) Summary() string { return fmt.Sprintf("%x", m.h.Sum(nil)) }
