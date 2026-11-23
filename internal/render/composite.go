// Package render composites video frames, audio blocks and transitions on
// the shared media timebase and emits deterministic PNG frames with hashes.
package render

import (
	"mediasim/internal/clock"
)

// Bounds are the fixed output geometry. Every run uses the same size so
// hashes are comparable across runs and fixtures.
const (
	Width  = 160
	Height = 90

	MaxFrames = 400 // guard rail against unbounded output
)

// DefaultParams are the deterministic render parameters recorded in the
// report. The seed changes the synthetic pattern; defaults keep demos stable.
type Params struct {
	Width  int   `json:"width"`
	Height int   `json:"height"`
	Seed   int64 `json:"seed"`
}

// DefaultParams returns the fixed geometry for the demo.
func DefaultParams(seed int64) Params {
	return Params{Width: Width, Height: Height, Seed: seed}
}

// Validate enforces the fixed geometry bounds.
func (p Params) Validate() error {
	if p.Width <= 0 || p.Height <= 0 || p.Width > 4096 || p.Height > 2160 {
		return clock.Errorf(clock.CatArgument, clock.Span{}, clock.RecoveredNone,
			"render size %dx%d outside 1..4096 x 1..2160", p.Width, p.Height)
	}
	if p.Width != Width || p.Height != Height {
		return clock.Errorf(clock.CatArgument, clock.Span{}, clock.RecoveredNone,
			"render size must be %dx%d for comparable hashes", Width, Height)
	}
	return nil
}

// LayerKind identifies what a layer contributes.
type LayerKind string

const (
	LayerVideo      LayerKind = "video"
	LayerAudio      LayerKind = "audio"
	LayerMarker     LayerKind = "marker"
	LayerTransition LayerKind = "transition"
)

// Layer is one visual contribution to a frame.
type Layer struct {
	Kind   LayerKind
	Source string
	Weight float64 // 0..1 crossfade weight
	Frame  int64   // source frame index
	Label  string  // marker/transition label
	Silent bool    // audio layer contributed silence
}

// Frame is the input to the compositor for one output index.
type Frame struct {
	Index   int
	Ticks   int64
	Layers  []Layer
	Audio   []AudioBlock
	DriftMs float64 // measured A/V drift for the report
}

// AudioBlock is a sample-exact slice of an audio track.
type AudioBlock struct {
	Source   string
	Start    int64
	Samples  []int16
	Silenced int // leading samples padded with silence
}

// SilenceSample is the fixed fill for cut/absent audio.
const SilenceSample int16 = 0
