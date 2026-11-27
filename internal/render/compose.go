// Package render composites tracks into 25 fps output frames and exports
// them atomically. Memory stays bounded: one canvas and two source frames
// are alive at a time, and each output frame is committed before the next
// is decoded.
package render

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"

	"github.com/disintegration/imaging"

	"fieldcut/internal/media"
	"fieldcut/internal/timeline"
)

// Layer is one source frame painted onto the canvas. When Secondary is set
// the layer is an interpolated frame mixed at Mix (weight of Secondary).
type Layer struct {
	Track     string
	Seq       int64
	Primary   image.Image
	Secondary image.Image
	Mix       media.Rational
	Mask      timeline.Rect
}

// MissingLayer records a layer that had no usable frame; the canvas keeps
// the background there rather than inventing pixels.
type MissingLayer struct {
	Track  string
	Seq    int64
	Reason string
}

// Composite paints layers in caller-supplied order (lowest priority first).
// Masks are clipped to the canvas; a zero mask means the whole canvas.
func Composite(w, h int, layers []Layer) (*image.NRGBA, []MissingLayer, error) {
	if w <= 0 || h <= 0 || w > media.MaxWidth || h > media.MaxHeight {
		return nil, nil, media.Newf(media.KindLimit, "render.compose", "", -1, "canvas %dx%d out of range", w, h)
	}
	dst := imaging.New(w, h, color.NRGBA{R: 16, G: 16, B: 18, A: 255})
	var missing []MissingLayer
	for _, l := range layers {
		if l.Primary == nil {
			missing = append(missing, MissingLayer{Track: l.Track, Seq: l.Seq, Reason: "no-frame"})
			continue
		}
		src := l.Primary
		if l.Secondary != nil {
			var err error
			src, err = Blend(l.Primary, l.Secondary, l.Mix)
			if err != nil {
				return nil, nil, media.New(media.KindCorrupt, "render.blend", l.Track, l.Seq, err)
			}
		}
		clip := timeline.Rect{X: 0, Y: 0, W: w, H: h}
		if !l.Mask.Empty() {
			clip = clip.Intersect(l.Mask)
		}
		if clip.Empty() {
			continue
		}
		if src.Bounds().Dx() != w || src.Bounds().Dy() != h {
			return nil, nil, media.Newf(media.KindCorrupt, "render.compose", l.Track, l.Seq,
				"source %dx%d does not match canvas %dx%d", src.Bounds().Dx(), src.Bounds().Dy(), w, h)
		}
		draw.Draw(dst, image.Rect(clip.X, clip.Y, clip.X+clip.W, clip.Y+clip.H),
			src, image.Point{X: clip.X, Y: clip.Y}, draw.Src)
	}
	return dst, missing, nil
}

// Blend mixes two equally sized frames with an exact rational weight on
// the second image: out = (a*(den-num) + b*num) / den, rounded once.
func Blend(a, b image.Image, mix media.Rational) (*image.NRGBA, error) {
	if mix.Den <= 0 || mix.Num < 0 || mix.Num > mix.Den {
		return nil, fmt.Errorf("mix %s outside [0,1]", mix)
	}
	if a.Bounds().Size() != b.Bounds().Size() {
		return nil, errors.New("blend operands differ in size")
	}
	bounds := a.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	out := imaging.New(w, h, color.NRGBA{})
	na, nb, den := int64(mix.Den-mix.Num), int64(mix.Num), int64(mix.Den)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r1, g1, b1, a1 := a.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			r2, g2, b2, a2 := b.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			pick := func(v1, v2 uint32) uint8 {
				sum := int64(v1/256)*na + int64(v2/256)*nb
				return uint8((sum + den/2) / den)
			}
			out.SetNRGBA(x, y, color.NRGBA{R: pick(r1, r2), G: pick(g1, g2), B: pick(b1, b2), A: pick(a1, a2)})
		}
	}
	return out, nil
}

// LoadFrame decodes a PNG and checks it against the manifest geometry.
func LoadFrame(path string, w, h int) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	if img.Bounds().Dx() != w || img.Bounds().Dy() != h {
		return nil, fmt.Errorf("frame is %dx%d, want %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), w, h)
	}
	return img, nil
}

// Encode writes a frame as PNG to w. PNG output is deterministic for the
// same pixels, which is what makes resume hashes equal full-run hashes.
func Encode(w *os.File, img image.Image) error {
	if err := png.Encode(w, img); err != nil {
		return err
	}
	return nil
}

// frameFileName is the canonical output name for a frame index.
func frameFileName(seq int64) string { return filepath.Join("frames", fmt.Sprintf("%05d.png", seq)) }
