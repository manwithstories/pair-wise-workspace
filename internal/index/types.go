// Package index rebuilds a random-access index from sample tables and
// verifies that seeks land on real samples in decode order.
package index

import (
	"mediad/internal/clock"
)

// Entry is one seek point. An entry always names a real sync sample: the
// index never points at a sample that was not delivered.
type Entry struct {
	// SampleIndex is the position of the sync sample in decode order.
	SampleIndex int `json:"sample_index"`
	// Time is the presentation timestamp of the sync sample.
	Time clock.Rational `json:"time"`
	// DTS is the decode timestamp, retained so a seek can verify the index
	// against decode order.
	DTS clock.Rational `json:"dts"`
	// Offset and Size locate the sample payload.
	Offset int64  `json:"offset"`
	Size   uint32 `json:"size"`
	// Damaged marks a seek point whose payload is incomplete. Such entries
	// are retained but flagged so a player can pick a neighbour instead.
	Damaged bool `json:"damaged"`
}

// TrackIndex is the seek index for one track.
type TrackIndex struct {
	TrackID uint32 `json:"track_id"`
	// Timescale is the denominator used for reported ticks.
	Timescale uint32  `json:"timescale"`
	Entries   []Entry `json:"entries"`
	// Skipped counts sync samples excluded because they were damaged or
	// synthesised; they are reported rather than silently indexed.
	Skipped int `json:"skipped"`
}

// SeekResult is the outcome of a sample seek.
type SeekResult struct {
	// Target is the requested time.
	Target clock.Rational `json:"target"`
	// Entry is the chosen sync sample.
	Entry Entry `json:"entry"`
	// Mismatch reports why a seek was rejected, if it was.
	Mismatch string `json:"mismatch,omitempty"`
}

// Rebuild builds the index for a repaired track.
