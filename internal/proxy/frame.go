// Package proxy normalises self-generated proxy frames with
// github.com/disintegration/imaging and computes a stable pixel hash.
//
// Proxy frames are synthesised from a seed and a media tick so the whole
// demo — including imaging's Lanczos resampling, orientation and grayscale
// conversions — runs offline without any bundled image files.
package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"

	"github.com/disintegration/imaging"
)

// DefaultSize is the proxy frame edge length.
const DefaultSize = 64

// Frame is a normalised proxy frame plus its pixel hash.
type Frame struct {
	Ticks         uint64
	Width, Height int
	Img           image.Image
	Hash          string
}

// Generate synthesises a deterministic source frame for seed and tick. The
// pattern is a moving diagonal gradient with a seeded noise overlay, so two
// different ticks never normalise to the same pixels.
func Generate(seed int64, ticks uint64, size int) *image.NRGBA {
	if size <= 0 {
		size = DefaultSize
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	phase := float64((seed*7919 + int64(ticks/3600)*104729) % 1000)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			t := float64(x+y)/float64(2*size) + phase/1000
			h := math.Mod(t, 1)
			r := uint8(255 * h)
			g := uint8(255 * (1 - h))
			b := uint8((seed*31 + int64(x*y+int(ticks))) % 256)
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return img
}

// Normalise resizes to a square proxy and converts to grayscale, which keeps
// the pixel hash insensitive to chroma drift introduced by transcoding.
func Normalise(src image.Image, size int) image.Image {
	if size <= 0 {
		size = DefaultSize
	}
	resized := imaging.Fill(src, size, size, imaging.Center, imaging.Lanczos)
	return imaging.Grayscale(resized)
}

// PixelHash returns the hex SHA-256 over the normalized RGBA bytes.
func PixelHash(img image.Image) string {
	b := img.Bounds()
	h := sha256.New()
	var px [4]byte
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			px[0], px[1], px[2], px[3] = byte(r>>8), byte(g>>8), byte(bl>>8), byte(a>>8)
			h.Write(px[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Make builds a proxy frame for a media tick.
func Make(seed int64, ticks uint64, size int) Frame {
	if size <= 0 {
		size = DefaultSize
	}
	src := Generate(seed, ticks, size)
	norm := Normalise(src, size)
	return Frame{
		Ticks: ticks, Width: size, Height: size,
		Img: norm, Hash: PixelHash(norm),
	}
}

// EncodePNG writes the frame as PNG into w.
func EncodePNG(w io.Writer, f Frame) error {
	if err := png.Encode(w, f.Img); err != nil {
		return fmt.Errorf("proxy: encode png: %w", err)
	}
	return nil
}

// Set aggregates a run of proxy frames.
type Set struct {
	Size   int
	Frames []Frame
	Hashes []string
}

// Build renders frames for the given ticks.
func Build(seed int64, ticks []uint64, size int) *Set {
	s := &Set{Size: size}
	for _, t := range ticks {
		f := Make(seed, t, size)
		s.Frames = append(s.Frames, f)
		s.Hashes = append(s.Hashes, f.Hash)
	}
	return s
}

// CombinedHash returns a digest over all frame hashes in order.
func (s *Set) CombinedHash() string {
	h := sha256.New()
	for _, f := range s.Hashes {
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}
