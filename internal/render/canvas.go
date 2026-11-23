package render

import (
	"image"
	"image/color"

	"mediasim/internal/clock"
)

// Canvas is an RGB image reused across frames to avoid allocation churn.
type Canvas struct {
	img    *image.RGBA
	Params Params
}

// NewCanvas validates params and allocates the fixed-size surface.
func NewCanvas(p Params) (*Canvas, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &Canvas{img: image.NewRGBA(image.Rect(0, 0, p.Width, p.Height)), Params: p}, nil
}

// Composite draws f into the canvas off-screen. Nothing touches a window or
// a display; the result is a plain image.
func (c *Canvas) Composite(f Frame) (*image.RGBA, error) {
	if f.Index < 0 || f.Index >= MaxFrames {
		return nil, clock.Errorf(clock.CatRender,
			clock.NewSpan("", "", clock.Interval{Start: f.Ticks, End: f.Ticks}), clock.RecoveredSkipped,
			"frame index %d outside 0..%d", f.Index, MaxFrames-1)
	}
	img := c.img
	// Base gradient derived deterministically from frame index and seed, so
	// empty frames are still stable and distinguishable.
	for y := 0; y < c.Params.Height; y++ {
		for x := 0; x < c.Params.Width; x++ {
			v := uint8((x*3 + y*5 + f.Index*7 + int(c.Params.Seed)) & 0xff)
			img.SetRGBA(x, y, color.RGBA{R: v, G: uint8(y + f.Index), B: uint8(x), A: 255})
		}
	}
	// Video layers: a block per source offset by the layer's frame index.
	col := 0
	for _, l := range f.Layers {
		switch l.Kind {
		case LayerVideo:
			c.drawBlock(col*12, 4, int(l.Frame)%(c.Params.Height-20), tintFor(l.Source))
			col++
		case LayerMarker:
			c.drawBlock(c.Params.Width-20, 6, int(l.Frame)%8, color.RGBA{255, 210, 0, 255})
		case LayerTransition:
			c.drawBlock(c.Params.Width/2-6, c.Params.Height/2-6, 12, color.RGBA{255, 255, 255, 255})
		}
	}
	return img, nil
}

func (c *Canvas) drawBlock(x, y, size int, col color.RGBA) {
	maxX := x + 12
	maxY := y + size
	if maxX > c.Params.Width {
		maxX = c.Params.Width
	}
	if maxY > c.Params.Height {
		maxY = c.Params.Height
	}
	for yy := y; yy < maxY; yy++ {
		for xx := x; xx < maxX; xx++ {
			if xx >= 0 && yy >= 0 {
				c.img.SetRGBA(xx, yy, col)
			}
		}
	}
}

func tintFor(source string) color.RGBA {
	var h uint32
	for i := 0; i < len(source); i++ {
		h = h*31 + uint32(source[i])
	}
	return color.RGBA{R: uint8(h), G: uint8(h >> 8), B: uint8(h >> 16), A: 255}
}
