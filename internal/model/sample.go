package model

import (
	"mediad/internal/clock"
)

// Sample is one coded media unit with its timing and provenance.
//
// DTS is the decode timestamp in timeline seconds and PTS the presentation
// timestamp. They are held as exact rationals so that an index rebuilt from
// them reproduces the container's edit list to the tick.
type Sample struct {
	Index int `json:"index"`
	// Offset and Size locate the sample payload inside its source file.
	Offset int64  `json:"offset"`
	Size   uint32 `json:"size"`

	DTS      clock.Rational `json:"dts"`
	PTS      clock.Rational `json:"pts"`
	Duration clock.Rational `json:"duration"`

	// Keyframe marks a sync sample; a random-access seek may only land here.
	Keyframe bool `json:"keyframe"`
	// Synthesised is true when no original frame existed and the pipeline
	// reconstructed one from an intact neighbour. Guessed frames are never
	// silently presented as original data.
	Synthesised bool `json:"synthesised"`
	// Damaged is true when the payload is known to be incomplete.
	Damaged bool `json:"damaged"`
}

// End returns DTS+Duration.
func (s Sample) End() (clock.Rational, error) { return s.DTS.Add(s.Duration) }

// TrackSamples is the per-track sample list handed from probe to repair to
// index.
type TrackSamples struct {
	TrackID uint32   `json:"track_id"`
	Samples []Sample `json:"samples"`
	// OriginalOffset is the offset added back after drift repair so callers
	// can compare before and after.
	OriginalOffset clock.Rational `json:"original_offset"`
	// RepairedOffset is the total offset applied by timestamp repair.
	RepairedOffset clock.Rational `json:"repaired_offset"`
	// Wraps counts timestamps that appeared to wrap past the 32-bit field.
	Wraps int `json:"wraps"`
	// Duplicates counts samples whose timestamp collided with the previous.
	Duplicates int `json:"duplicates"`
}

// Len returns the sample count.
func (t TrackSamples) Len() int { return len(t.Samples) }

// SampleReader resolves a sample's bytes from its source.
type SampleReader interface {
	// ReadSample copies sample payload into dst and returns bytes read.
	ReadSample(sample Sample, dst []byte) (int, error)
}
