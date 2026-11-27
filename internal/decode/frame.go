package decode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"

	"github.com/disintegration/imaging"

	"archive/internal/media"
)

// normaliseStream decodes the PNG byte stream from FFmpeg into RGBA frames,
// resizing each frame to canonical dimensions when needed so representation
// layers compare pixel-for-pixel.
func normaliseStream(r io.Reader, width, height int, samples []InputSample) ([]Frame, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("canonical dimensions %dx%d invalid", width, height)
	}
	var out []Frame
	idx := 0
	for {
		img, err := png.Decode(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			if idx == 0 {
				return nil, fmt.Errorf("decode first frame: %w", err)
			}
			break
		}
		start := int64(0)
		if idx < len(samples) {
			start = samples[idx].StartUS
		} else if len(samples) > 0 {
			start = samples[len(samples)-1].StartUS + samples[len(samples)-1].DurationUS
		}
		norm := Normalise(img, width, height)
		norm.Index = idx
		norm.StartUS = start
		out = append(out, norm)
		idx++
		if idx > len(samples)*4+64 {
			return nil, fmt.Errorf("decoder produced %d frames for %d samples", idx, len(samples))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("decoder produced no frames")
	}
	return out, nil
}

// Normalise converts any image to tightly packed RGBA at exact dimensions.
func Normalise(img image.Image, width, height int) Frame {
	b := img.Bounds()
	if b.Dx() != width || b.Dy() != height {
		img = imaging.Resize(img, width, height, imaging.Lanczos)
		b = img.Bounds()
	}
	pix := make([]byte, width*height*4)
	i := 0
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			cr, cg, cb, ca := rgbaAt(img, b.Min.X+x, b.Min.Y+y)
			pix[i+0] = cr
			pix[i+1] = cg
			pix[i+2] = cb
			pix[i+3] = ca
			i += 4
		}
	}
	return Frame{Pixels: pix, Width: width, Height: height}
}

func rgbaAt(img image.Image, x, y int) (uint8, uint8, uint8, uint8) {
	r, g, b, a := img.At(x, y).RGBA()
	return uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)
}

// EncodeFramePNG renders a normalised frame as PNG at canonical size.
func EncodeFramePNG(f Frame) ([]byte, error) {
	if len(f.Pixels) != f.Width*f.Height*4 {
		return nil, fmt.Errorf("frame %d pixel buffer %d != %d", f.Index, len(f.Pixels), f.Width*f.Height*4)
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, &rgbaImage{Pix: f.Pixels, W: f.Width, H: f.Height}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// rgbaImage is a zero-copy image.Image view over a packed RGBA buffer.
type rgbaImage struct {
	Pix  []byte
	W, H int
}

func (m *rgbaImage) ColorModel() color.Model { return color.RGBAModel }

func (m *rgbaImage) Bounds() image.Rectangle { return image.Rect(0, 0, m.W, m.H) }

func (m *rgbaImage) At(x, y int) color.Color {
	if x < 0 || y < 0 || x >= m.W || y >= m.H {
		return color.RGBA{}
	}
	off := (y*m.W + x) * 4
	return color.RGBA{R: m.Pix[off], G: m.Pix[off+1], B: m.Pix[off+2], A: m.Pix[off+3]}
}

// binary is retained for the Annex-B helper in ffmpeg.go's package build.
var _ = binary.BigEndian
var _ = media.KindVideo
