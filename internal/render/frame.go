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
}

// NewEngine creates a rendering engine.
func NewEngine(seed int64) *Engine { return &Engine{Seed: seed} }

// sourceColor is the single canonical source function. Both the forward raster
// and the inverse-mapped reference sample it, so structural comparison isolates
// geometry and color handling rather than re-implementing the pattern.
func sourceColor(seed int64, trackKey string, index int64, w, h, x, y int) color.RGBA {
	// Mix the full key, not just its length: "v0" and "v1" must differ.
	keyHash := sha256.Sum256([]byte(trackKey))
	mix := uint64(seed)*0x9E3779B97F4A7C15 +
		uint64(keyHash[0])<<48 | uint64(keyHash[1])<<40 | uint64(index+1)*7919
	// Integer-only deterministic pattern: no floating accumulation.
	v := uint64(x*13+y*7+int(index)*31) + mix
	v ^= v >> 13
	v *= 0x5DEECE66D
	v ^= v >> 17
	r := uint8(v >> 8)
	g := uint8((uint64(x)*255)/uint64(w) + uint64(y)*3)
	b := uint8((uint64(y)*255)/uint64(h) + uint64(index)*11)
	c := color.RGBA{R: r, G: g, B: b, A: 255}
	// A stable marker block at a sample-dependent offset makes frame
	// differences detectable.
	bx := int((int64(w) / 4) + ((index % 5) * int64(w) / 20))
	by := int((int64(h) / 4) + ((index % 3) * int64(h) / 12))
	if x >= bx && x < bx+w/8+1 && y >= by && y < by+h/8+1 {
		c = color.RGBA{R: uint8(mix >> 3), G: uint8(mix >> 11), B: uint8(mix >> 19), A: 255}
	}
	return c
}

// Source returns the deterministic, unrotated source picture for a track and
// sample index.
func (e *Engine) Source(trackKey string, index int64, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if w <= 0 || h <= 0 {
		return img
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, sourceColor(e.Seed, trackKey, index, w, h, x, y))
		}
	}
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
//
// For 90: out(srcH-1-srcY, srcX) = src(srcX, srcY).
// For 270: out(srcY, srcW-1-srcX) = src(srcX, srcY).
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

// colorMatrix returns the target gamut/transfer matrix for a color
// description. The demo models the color stage as a linear 3x3 gamut
// conversion plus a per-channel transfer gain; both are deterministic.
func colorMatrix(c metadata.Color) (m [3][3]float64, gain [3]float64) {
	m = [3][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	gain = [3]float64{1, 1, 1}
	switch c.Primaries {
	case metadata.PrimariesBT2020:
		m = [3][3]float64{{0.82, 0.12, 0.06}, {0.05, 0.90, 0.05}, {0.02, 0.08, 0.90}}
	case metadata.PrimariesP3:
		m = [3][3]float64{{0.95, 0.04, 0.01}, {0.02, 0.97, 0.01}, {0.01, 0.04, 0.95}}
	}
	switch c.Transfer {
	case metadata.TransferPQ:
		gain = [3]float64{0.88, 0.88, 0.88}
	case metadata.TransferSRGB:
		gain = [3]float64{0.96, 0.96, 0.96}
	}
	return m, gain
}

// identityColor reports whether the color stage is a no-op.
func identityColor(c metadata.Color) bool {
	return c.Source == metadata.SourceDefault && c.Primaries == metadata.PrimariesBT709 &&
		c.Transfer == metadata.TransferBT709 && c.Matrix == metadata.MatrixBT709 && !c.FullRange
}

// transform applies the color transform to one pixel.
func transform(p color.RGBA, c metadata.Color, exact bool) color.RGBA {
	if identityColor(c) {
		return p
	}
	m, gain := colorMatrix(c)
	in := [3]float64{float64(p.R), float64(p.G), float64(p.B)}
	var res [3]float64
	for i := 0; i < 3; i++ {
		res[i] = (m[i][0]*in[0] + m[i][1]*in[1] + m[i][2]*in[2]) * gain[i]
	}
	round := 0.5
	if exact {
		round = 0
	}
	return color.RGBA{
		R: uint8(clamp8(int(res[0] + round))),
		G: uint8(clamp8(int(res[1] + round))),
		B: uint8(clamp8(int(res[2] + round))),
		A: 255,
	}
}

// ApplyColor applies the chosen color transform with 8-bit rounding. It is the
// deterministic production path.
func ApplyColor(src *image.RGBA, c metadata.Color) *image.RGBA {
	return applyColor(src, c, false)
}

// ApplyColorExact is the floating-point reference used by the fidelity gate.
// The production path rounds to 8 bits; this path does not, so PSNR isolates
// quantization error of an otherwise identical transform.
func ApplyColorExact(src *image.RGBA, c metadata.Color) *image.RGBA {
	return applyColor(src, c, true)
}

func applyColor(src *image.RGBA, c metadata.Color, exact bool) *image.RGBA {
	if identityColor(c) {
		return src
	}
	out := image.NewRGBA(src.Bounds())
	for y := src.Bounds().Min.Y; y < src.Bounds().Max.Y; y++ {
		for x := src.Bounds().Min.X; x < src.Bounds().Max.X; x++ {
			out.SetRGBA(x, y, transform(src.RGBAAt(x, y), c, exact))
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

type colorMode int

const (
	colorRounded colorMode = iota
	colorExact
)

// RenderFrame composites one planned frame with the rounded color transform.
func (e *Engine) RenderFrame(f timeline.Frame) (RenderedFrame, error) {
	return e.renderFrame(f, colorRounded)
}

// RenderReference composites the same plan with the floating-point color path.
// Geometry, sources and the color matrix are identical, so the only possible
// difference is 8-bit quantization.
func (e *Engine) RenderReference(f timeline.Frame) (RenderedFrame, error) {
	return e.renderFrame(f, colorExact)
}

func (e *Engine) renderFrame(f timeline.Frame, mode colorMode) (RenderedFrame, error) {
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
		codedW, codedH, crop := rasterGeometry(l)
		src := e.Source(l.TrackKey, l.SourceIndex, codedW, codedH)
		out.LayerHashes = append(out.LayerHashes, HashImage(src))
		src = Crop(src, &crop)
		src = Rotate(src, l.Rotation)
		if mode == colorExact {
			src = ApplyColorExact(src, l.Color)
		} else {
			src = ApplyColor(src, l.Color)
		}
		blit(canvas, src, l.X, l.Y)
	}
	if len(layers) == 0 {
		// A hole that is not blocked is represented explicitly, never left as
		// uninitialized memory.
		fillRect(canvas, 0, 0, w, h, color.RGBA{A: 255})
	}
	out.Image = canvas
	out.Hash = frameHash(f, canvas, out.LayerHashes)
	return out, nil
}

// InverseSample composes a frame by inverse mapping: for each output pixel it
// undoes the layer translation, rotation and crop, then samples the source
// function directly. It shares no forward-blit code with RenderFrame and is
// used as an independent structural reference.
//
// The forward path applies the rounded color transform; the reference uses the
// exact floating-point path, so the only intended difference is 8-bit
// quantization. Any geometry error collapses the score.
func (e *Engine) InverseSample(f timeline.Frame) *image.RGBA {
	canvas := image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
	if f.Blocked || len(f.Layers) == 0 {
		fillRect(canvas, 0, 0, f.Width, f.Height, color.RGBA{A: 255})
		return canvas
	}
	layers := append([]timeline.Layer(nil), f.Layers...)
	sort.SliceStable(layers, func(i, j int) bool { return layers[i].TrackKey < layers[j].TrackKey })
	for _, l := range layers {
		codedW, codedH, crop := rasterGeometry(l)
		// The pre-rotation raster is the raster crop rectangle.
		srcW, srcH := crop.W, crop.H
		for oy := 0; oy < l.H; oy++ {
			for ox := 0; ox < l.W; ox++ {
				var rx, ry int
				switch l.Rotation {
				case 90:
					rx, ry = oy, srcH-1-ox
				case 180:
					rx, ry = srcW-1-ox, srcH-1-oy
				case 270:
					rx, ry = srcW-1-oy, ox
				default:
					rx, ry = ox, oy
				}
				if rx < 0 || ry < 0 || rx >= srcW || ry >= srcH {
					continue
				}
				sx, sy := crop.X+rx, crop.Y+ry
				if sx < 0 || sy < 0 || sx >= codedW || sy >= codedH {
					continue
				}
				p := transform(sourceColor(e.Seed, l.TrackKey, l.SourceIndex, codedW, codedH, sx, sy),
					l.Color, true)
				dx, dy := l.X+ox, l.Y+oy
				if dx >= 0 && dy >= 0 && dx < f.Width && dy < f.Height {
					canvas.SetRGBA(dx, dy, p)
				}
			}
		}
	}
	return canvas
}

// sourceSize returns the coded (pre-crop, pre-rotation) size of a layer.
func sourceSize(l timeline.Layer) (int, int) {
	if l.CodedW > 0 && l.CodedH > 0 {
		return l.CodedW, l.CodedH
	}
	return l.W, l.H
}

// rasterGeometry returns the coded source size and crop rectangle in the raster
// actually being rendered. Layer.W/H are post-rotation output dimensions, and
// the model's crop/coded values are full resolution; a verification render
// downscales the output, so the same factor is applied to the source raster and
// crop. Both forward and inverse paths use this helper, keeping them
// geometrically identical.
func rasterGeometry(l timeline.Layer) (codedW, codedH int, crop metadata.Crop) {
	codedW, codedH = sourceSize(l)
	if l.Crop != nil {
		crop = *l.Crop
	} else {
		crop = metadata.Crop{X: 0, Y: 0, W: codedW, H: codedH}
	}
	if crop.W <= 0 || crop.H <= 0 {
		return 1, 1, metadata.Crop{W: 1, H: 1}
	}
	// Pre-rotation raster dimensions follow from the post-rotation output.
	preW, preH := l.W, l.H
	if l.Rotation == 90 || l.Rotation == 270 {
		preW, preH = l.H, l.W
	}
	if preW <= 0 || preH <= 0 {
		preW, preH = crop.W, crop.H
	}
	// The pre-rotation raster is the crop rendered at the raster's scale. When
	// there is no model crop, the raster is the output with the quarter turn
	// undone; scale both the source and the crop rectangle into that raster.
	scaleExtent := func(v, raster, full int) int {
		if full <= 0 || raster <= 0 || raster == full {
			return v
		}
		out := int(int64(v) * int64(raster) / int64(full))
		if out < 1 {
			out = 1
		}
		return out
	}
	scaleOffset := func(v, raster, full int) int {
		if full <= 0 || raster <= 0 || raster == full {
			return v
		}
		return int(int64(v) * int64(raster) / int64(full))
	}
	// Only extents scale with the raster. The crop's X/Y are an offset into the
	// coded frame, and both the source raster and the crop stay in the same
	// zero-origin coordinate system, so a uniform shrink keeps x/y relative to
	// the shrink factor rather than to the (already scaled) crop width.
	fullCropW, fullCropH := crop.W, crop.H
	fullCodedW, fullCodedH := codedW, codedH
	crop.X = scaleOffset(crop.X, preW, fullCodedW)
	crop.Y = scaleOffset(crop.Y, preH, fullCodedH)
	crop.W = scaleExtent(fullCropW, preW, fullCropW)
	crop.H = scaleExtent(fullCropH, preH, fullCropH)
	codedW = scaleExtent(fullCodedW, preW, fullCropW)
	codedH = scaleExtent(fullCodedH, preH, fullCropH)
	return codedW, codedH, crop
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
