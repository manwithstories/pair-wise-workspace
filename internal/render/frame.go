// Package render produces frames entirely offscreen with no window, browser,
// camera or microphone dependency. Source pictures are procedural and
// deterministic for a given seed and source index, so repeated runs yield
// identical SHA-256 hashes.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"math"
	"sort"

	"mediad/internal/metadata"
	"mediad/internal/model"
	"mediad/internal/timeline"
)

// Engine renders timeline frames offscreen.
type Engine struct {
	Seed int64
	// Scale divides coded dimensions for faster verification while keeping
	// geometry rules exact. 1 renders at full resolution.
	Scale int
}

// NewEngine creates a rendering engine.
func NewEngine(seed int64) *Engine { return &Engine{Seed: seed, Scale: 1} }

// Source returns the deterministic, unrotated source picture for a track and
// sample index. The pattern combines a moving gradient with track-specific
// blocks so different tracks are visually and statistically distinct.
func (e *Engine) Source(trackKey string, index int64, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if w <= 0 || h <= 0 {
		return img
	}
	// Mix the full key, not just its length: "v0" and "v1" must differ.
	keyHash := sha256.Sum256([]byte(trackKey))
	seed := uint64(e.Seed)*0x9E3779B97F4A7C15 +
		uint64(keyHash[0])<<48 | uint64(keyHash[1])<<40 | uint64(index+1)*7919
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Integer-only deterministic pattern: no floating accumulation.
			v := uint64(x*13+y*7+int(index)*31) + seed
			v ^= v >> 13
			v *= 0x5DEECE66D
			v ^= v >> 17
			r := uint8(v >> 8)
			g := uint8((uint64(x)*255)/uint64(w) + uint64(y)*3)
			b := uint8((uint64(y)*255)/uint64(h) + uint64(index)*11)
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	// A stable marker block at a sample-dependent offset makes frame
	// differences detectable.
	bx := int((int64(w) / 4) + ((index % 5) * int64(w) / 20))
	by := int((int64(h) / 4) + ((index % 3) * int64(h) / 12))
	marker := color.RGBA{R: uint8(seed >> 3), G: uint8(seed >> 11), B: uint8(seed >> 19), A: 255}
	fillRect(img, bx, by, w/8+1, h/8+1, marker)
	return img
}

func fillRect(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	b := img.Bounds()
	for yy := y; yy < y+h; yy++ {
		if yy < b.Min.Y || yy >= b.Max.Y {
			continue
		}
		for xx := x; xx < x+w; xx++ {
			if xx < b.Min.X || xx >= b.Max.X {
				continue
			}
			img.SetRGBA(xx, yy, c)
		}
	}
}

// Crop returns the clean aperture sub-image.
func Crop(src *image.RGBA, c *metadata.Crop) *image.RGBA {
	if c == nil {
		return src
	}
	b := src.Bounds()
	r := image.Rect(c.X, c.Y, c.X+c.W, c.Y+c.H).Intersect(b)
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			out.SetRGBA(x, y, src.RGBAAt(r.Min.X+x, r.Min.Y+y))
		}
	}
	return out
}

// Rotate rotates clockwise by 0, 90, 180 or 270 degrees into a new image.
func Rotate(src *image.RGBA, rotation int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	switch rotation {
	case 90, 270:
		out := image.NewRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				c := src.RGBAAt(x, y)
				if rotation == 90 {
					out.SetRGBA(h-1-y, x, c)
				} else {
					out.SetRGBA(y, w-1-x, c)
				}
			}
		}
		return out
	case 180:
		out := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.SetRGBA(w-1-x, h-1-y, src.RGBAAt(x, y))
			}
		}
		return out
	default:
		out := image.NewRGBA(image.Rect(0, 0, w, h))
		copy(out.Pix, src.Pix)
		return out
	}
}

// ApplyColor applies the chosen color transform. Only gamut/range handling is
// performed; transfer decoding is a no-op for BT.709 and a linear gain for
// PQ/sRGB so the transform is visible but deterministic.
func ApplyColor(src *image.RGBA, c metadata.Color) *image.RGBA {
	if c.Source == metadata.SourceDefault && c.Primaries == metadata.PrimariesBT709 &&
		c.Transfer == metadata.TransferBT709 && c.Matrix == metadata.MatrixBT709 && !c.FullRange {
		return src
	}
	out := image.NewRGBA(src.Bounds())
	for y := src.Bounds().Min.Y; y < src.Bounds().Max.Y; y++ {
		for x := src.Bounds().Min.X; x < src.Bounds().Max.X; x++ {
			p := src.RGBAAt(x, y)
			r, g, b := int(p.R), int(p.G), int(p.B)
			switch c.Primaries {
			case metadata.PrimariesBT2020:
				// Approximate BT.2020 -> BT.709 gamut compression.
				r = clamp8(r*80/100 + b*5/100)
				g = clamp8(g * 92 / 100)
				b = clamp8(b * 85 / 100)
			case metadata.PrimariesP3:
				r = clamp8(r * 95 / 100)
				b = clamp8(b * 96 / 100)
			}
			switch c.Transfer {
			case metadata.TransferPQ:
				r = clamp8(r * 9 / 10)
				g = clamp8(g * 9 / 10)
				b = clamp8(b * 9 / 10)
			case metadata.TransferSRGB:
				r = clamp8(r * 97 / 100)
				g = clamp8(g * 97 / 100)
				b = clamp8(b * 97 / 100)
			}
			out.SetRGBA(x, y, color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255})
		}
	}
	return out
}

func clamp8(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

// RenderedFrame is the offscreen result for one timeline frame.
type RenderedFrame struct {
	Index      int
	Start, End int64
	Image      *image.RGBA
	// Hash is SHA-256 over canonical RGBA bytes and frame metadata.
	Hash string
	// Blocked and BlockingDep mirror the plan: blocked frames are not painted.
	Blocked     bool
	BlockingDep string
	// LayerHashes commit to each source layer before compositing.
	LayerHashes []string
}

// RenderFrame composites one planned frame. Blocked frames render a black
// placeholder (still deterministic) and carry the blocking dependency.
func (e *Engine) RenderFrame(f timeline.Frame) (RenderedFrame, error) {
	out := RenderedFrame{Index: f.Index, Start: f.Start, End: f.End,
		Blocked: f.Blocked, BlockingDep: f.BlockingDep}
	w, h := f.Width, f.Height
	if w <= 0 || h <= 0 {
		return out, model.New(model.CategoryRender, "frame", "empty canvas").
			WithBlockingDep(f.BlockingDep)
	}
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	if f.Blocked {
		fillRect(canvas, 0, 0, w, h, color.RGBA{A: 255})
		out.Image = canvas
		out.Hash = frameHash(f, canvas, nil)
		return out, nil
	}
	layers := append([]timeline.Layer(nil), f.Layers...)
	sort.SliceStable(layers, func(i, j int) bool { return layers[i].TrackKey < layers[j].TrackKey })
	for _, l := range layers {
		sw, sh := sourceSize(l)
		src := e.Source(l.TrackKey, l.SourceIndex, sw, sh)
		lh := HashImage(src)
		out.LayerHashes = append(out.LayerHashes, lh)
		src = Crop(src, l.Crop)
		src = Rotate(src, l.Rotation)
		src = ApplyColor(src, l.Color)
		blit(canvas, src, l.X, l.Y)
	}
	if len(layers) == 0 {
		// A hole that is not blocked is represented explicitly, not left as
		// uninitialized memory.
		fillRect(canvas, 0, 0, w, h, color.RGBA{A: 255})
	}
	out.Image = canvas
	out.Hash = frameHash(f, canvas, out.LayerHashes)
	return out, nil
}

// sourceSize returns the coded (pre-crop, pre-rotation) size of a layer.
// Layer.W/H are post-rotation output dimensions, so a quarter turn swaps them
// back before cropping.
func sourceSize(l timeline.Layer) (int, int) {
	w, h := l.W, l.H
	if l.Rotation == 90 || l.Rotation == 270 {
		w, h = h, w
	}
	return w, h
}

func blit(dst, src *image.RGBA, ox, oy int) {
	sb := src.Bounds()
	db := dst.Bounds()
	for y := 0; y < sb.Dy(); y++ {
		for x := 0; x < sb.Dx(); x++ {
			dx, dy := ox+x, oy+y
			if dx < db.Min.X || dx >= db.Max.X || dy < db.Min.Y || dy >= db.Max.Y {
				continue
			}
			dst.SetRGBA(dx, dy, src.RGBAAt(sb.Min.X+x, sb.Min.Y+y))
		}
	}
}

// HashImage returns the SHA-256 of the canonical RGBA bytes.
func HashImage(img *image.RGBA) string {
	h := sha256.New()
	h.Write([]byte{byte(img.Bounds().Dx() >> 24), byte(img.Bounds().Dx() >> 16),
		byte(img.Bounds().Dx() >> 8), byte(img.Bounds().Dx())})
	h.Write([]byte{byte(img.Bounds().Dy() >> 24), byte(img.Bounds().Dy() >> 16),
		byte(img.Bounds().Dy() >> 8), byte(img.Bounds().Dy())})
	h.Write(img.Pix)
	return hex.EncodeToString(h.Sum(nil))
}

func frameHash(f timeline.Frame, img *image.RGBA, layers []string) string {
	h := sha256.New()
	h.Write([]byte(f.PlanHash))
	for _, lh := range layers {
		h.Write([]byte(lh))
	}
	h.Write(img.Pix)
	return hex.EncodeToString(h.Sum(nil))
}

// PSNR computes peak signal-to-noise ratio in dB between two equally sized
// images. Identical images return 100 dB so callers never divide by zero.
func PSNR(a, b *image.RGBA) float64 {
	if a.Bounds() != b.Bounds() {
		return 0
	}
	var mse float64
	n := 0
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			pa := a.RGBAAt(x, y)
			pb := b.RGBAAt(x, y)
			for _, d := range [...]float64{
				float64(pa.R) - float64(pb.R),
				float64(pa.G) - float64(pb.G),
				float64(pa.B) - float64(pb.B),
			} {
				mse += d * d
			}
			n += 3
		}
	}
	if n == 0 || mse == 0 {
		return 100
	}
	mse /= float64(n)
	return 10 * math.Log10(255*255/mse)
}

// SSIM computes the mean structural similarity using 8x8 windows.
func SSIM(a, b *image.RGBA) float64 {
	if a.Bounds() != b.Bounds() {
		return 0
	}
	bounds := a.Bounds()
	const win = 8
	var total float64
	var windows int
	const c1 = 6.5025  // (0.01*255)^2
	const c2 = 58.5225 // (0.03*255)^2
	for wy := bounds.Min.Y; wy < bounds.Max.Y; wy += win {
		for wx := bounds.Min.X; wx < bounds.Max.X; wx += win {
			var sa, sb, saa, sbb, sab float64
			var n float64
			for y := wy; y < wy+win && y < bounds.Max.Y; y++ {
				for x := wx; x < wx+win && x < bounds.Max.X; x++ {
					pa, pb := a.RGBAAt(x, y), b.RGBAAt(x, y)
					for k := 0; k < 3; k++ {
						va := channel(pa, k)
						vb := channel(pb, k)
						sa += va
						sb += vb
						saa += va * va
						sbb += vb * vb
						sab += va * vb
						n++
					}
				}
			}
			if n == 0 {
				continue
			}
			ma, mb := sa/n, sb/n
			va := saa/n - ma*ma
			vb := sbb/n - mb*mb
			cov := sab/n - ma*mb
			num := (2*ma*mb + c1) * (2*cov + c2)
			den := (ma*ma + mb*mb + c1) * (va + vb + c2)
			if den != 0 {
				total += num / den
				windows++
			}
		}
	}
	if windows == 0 {
		return 1
	}
	return total / float64(windows)
}

func channel(c color.RGBA, k int) float64 {
	switch k {
	case 0:
		return float64(c.R)
	case 1:
		return float64(c.G)
	default:
		return float64(c.B)
	}
}
