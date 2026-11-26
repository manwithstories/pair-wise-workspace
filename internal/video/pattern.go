package video

import (
	"image"
	"math"

	"github.com/disintegration/imaging"

	"pairflow/internal/media"
)

// Pattern names a synthetic test pattern. Unknown names are errors.
const (
	PatternBars  = "bars"
	PatternRamp  = "ramp"
	PatternRings = "rings"
)

var tintLinear = map[string][3]float64{
	"teal":    {0.02, 0.42, 0.38},
	"amber":   {0.78, 0.40, 0.02},
	"neutral": {0.5, 0.5, 0.5},
}

// ValidatePattern rejects unknown pattern or tint names so a typo cannot turn
// into a blank frame.
func ValidatePattern(pattern, tint, path, field string) *media.Error {
	switch pattern {
	case PatternBars, PatternRamp, PatternRings:
	default:
		return media.Input("video.unknown_pattern",
			"unknown pattern %q (supported: bars, ramp, rings)", pattern).
			WithPath(path).WithField(field)
	}
	if _, ok := tintLinear[tint]; !ok {
		return media.Input("video.unknown_tint",
			"unknown tint %q (supported: teal, amber, neutral)", tint).
			WithPath(path).WithField(field)
	}
	return nil
}

// Pattern renders a 320x180 frame directly in the declared source encoding.
// The pattern is defined in linear light and encoded with the source transfer
// curve, so downstream Normalize() must undo exactly that curve. Pixels are
// written straight into the NRGBA buffer: the transfer curves are the dominant
// cost and per-pixel method calls would dominate a 72-frame render.
func Pattern(pattern, tint string, space Space, frame int) *image.NRGBA {
	const w, h = 320, 180
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	key := tintLinear[tint]
	if key == [3]float64{} && tint != "neutral" {
		key = tintLinear["neutral"]
	}
	bg := [3]float64{0.004, 0.005, 0.007}
	// A 256-entry transfer lookup per source encoding avoids recomputing
	// pow() three times for every pixel of every frame.
	enc := spaceLookup(space)
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			lin := patternLinear(pattern, x, y, frame, key, bg)
			row[x*4+0] = enc[linearToIndex(lin[0])]
			row[x*4+1] = enc[linearToIndex(lin[1])]
			row[x*4+2] = enc[linearToIndex(lin[2])]
			row[x*4+3] = 255
		}
	}
	return img
}

// spaceLookup builds a 256-step table of the source encoding applied to a
// uniformly sampled linear ramp.
func spaceLookup(s Space) [256]uint8 {
	var tab [256]uint8
	for i := 0; i < 256; i++ {
		t := float64(i) / 255
		tab[i] = encodeSpace(s, t*t) // square sampling keeps shadows accurate
	}
	return tab
}

// linearToIndex quantizes linear light into the lookup table's index space.
func linearToIndex(v float64) int {
	v = clamp01(v)
	idx := int(math.Sqrt(v) * 255)
	if idx > 255 {
		idx = 255
	}
	return idx
}

// encodeSpace applies the source transfer curve and quantizes to 8 bit.
func encodeSpace(s Space, linear float64) uint8 {
	if s.Name == "rec709" {
		return clamp8(math.Pow(clamp01(linear), 1/s.Gamma))
	}
	return clamp8(encodeSRGB(clamp01(linear)))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// patternLinear returns the linear-light RGB of one pixel.
func patternLinear(pattern string, x, y, frame int, key, bg [3]float64) [3]float64 {
	switch pattern {
	case PatternBars:
		bar := (x*8/320 + frame) % 8
		f := float64(bar) / 7.0
		return blendLin(bg, key, 0.15+0.85*f)
	case PatternRamp:
		v := float64((x+y+frame*3)%320) / 319.0
		c := blendLin(key, [3]float64{1, 1, 1}, v)
		return blendLin(bg, c, 0.8)
	case PatternRings:
		d := math.Hypot(float64(x-160), float64(y-90))
		v := (math.Sin(d/12.0-float64(frame)*0.35) + 1) / 2
		return blendLin(bg, key, 0.2+0.7*v)
	default:
		return bg
	}
}

func blendLin(a, b [3]float64, t float64) [3]float64 {
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return [3]float64{
		a[0] + (b[0]-a[0])*t,
		a[1] + (b[1]-a[1])*t,
		a[2] + (b[2]-a[2])*t,
	}
}

// Blend cross-dissolves two normalized sRGB frames with weight t in [0,1]:
// t=0 yields a, t=1 yields b. imaging supplies the compositing kernel.
func Blend(a, b *image.NRGBA, t float64) *image.NRGBA {
	if t <= 0 {
		return imaging.Clone(a)
	}
	if t >= 1 {
		return imaging.Clone(b)
	}
	return imaging.Overlay(a, b, image.Point{}, t)
}
