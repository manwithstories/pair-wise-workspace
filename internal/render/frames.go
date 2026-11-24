// Package render produces a deterministic offscreen review sequence: a
// small raster per review frame plus a manifest that ties every frame to the
// timeline events that produced it.
package render

import (
	"fmt"
	"image/color"

	"mediarun/internal/model"
)

// Limits keep the review raster small enough to render without surprises.
const (
	MaxWidth  = 320
	MaxHeight = 180
	MaxFrames = 512
)

// Config controls offscreen rendering.
type Config struct {
	Width  int
	Height int
}

// DefaultConfig returns a small deterministic raster size.
func DefaultConfig() Config { return Config{Width: 160, Height: 90} }

func (c Config) validate() error {
	if c.Width < 16 || c.Height < 16 {
		return fmt.Errorf("%w: raster %dx%d too small", model.ErrInvalid, c.Width, c.Height)
	}
	if c.Width > MaxWidth || c.Height > MaxHeight {
		return fmt.Errorf("%w: raster %dx%d exceeds %dx%d", model.ErrLimit, c.Width, c.Height, MaxWidth, MaxHeight)
	}
	return nil
}

// palette is fixed so PNG bytes are reproducible across runs.
var palette = [8]color.RGBA{
	{0x10, 0x10, 0x18, 0xff},
	{0x30, 0x80, 0xD0, 0xff},
	{0xD0, 0x90, 0x30, 0xff},
	{0x30, 0xC0, 0x70, 0xff},
	{0xC0, 0x40, 0x60, 0xff},
	{0x90, 0x60, 0xD0, 0xff},
	{0xE0, 0xE0, 0xE0, 0xff},
	{0x60, 0x60, 0x70, 0xff},
}
