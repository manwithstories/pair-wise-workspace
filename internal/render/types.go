package render

import (
	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// Frame is one rendered review raster.
type Frame struct {
	Index   int        `json:"index"`
	TrackID uint32     `json:"track_id"`
	Start   clock.Tick `json:"start"`
	Kind    model.Kind `json:"kind"`
	Label   string     `json:"label"`
	PNG     []byte     `json:"-"`
	SHA256  string     `json:"sha256"`
	Events  []string   `json:"events"`
}

// Sequence is the complete offscreen output.
type Sequence struct {
	Frames   []Frame
	Manifest []byte
	Width    int
	Height   int
}
