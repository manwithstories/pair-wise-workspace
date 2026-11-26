package render

import (
	"bytes"
	"image"
	"image/color"
	"image/png"

	"github.com/disintegration/imaging"
)

// DrawInput fully determines one frame's pixels. Nothing here reads wall
// clock time or ambient state, which is what makes frame hashes reproducible.
type DrawInput struct {
	Seed           uint64
	StreamID       int
	Width          int
	Height         int
	AtMaster       int64
	KeyframeMaster int64
	KeyframeSeq    int64
}

// hashMix is a splitmix64-style finalizer giving a well-distributed uint64
// from the frame identity tuple.
func hashMix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// DrawFrame renders a self-describing RGBA frame. A deterministic base
// gradient is overlaid with a block pattern seeded by the frame identity and
// a bright keyframe landing marker, so drift and seek position are visible
// in the image and reflected in its hash.
func DrawFrame(in DrawInput) *image.NRGBA {
	w, h := in.Width, in.Height
	if w < 8 {
		w = 8
	}
	if h < 8 {
		h = 8
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	base := hashMix(in.Seed ^ uint64(in.StreamID)*7919)
	frameID := hashMix(uint64(in.AtMaster) ^ base)
	block := 4
	if block > w/2 {
		block = w / 2
	}
	if block < 1 {
		block = 1
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Smooth vertical gradient from the frame identity.
			r := uint8((frameID>>24)&0xff) ^ uint8(y*255/h)
			g := uint8((frameID>>16)&0xff) ^ uint8(x*255/w)
			b := uint8((frameID>>8)&0xff) ^ uint8((x+y)*255/(w+h))
			// Checker blocks make the master tick visually inspectable.
			if ((x/block)+(y/block))&1 == 0 {
				r = (r + 64) & 0xff
				g = (g + 32) & 0xff
			}
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
		}
	}

	// Keyframe landing marker: a fixed-height band whose position encodes the
	// keyframe sequence, guaranteeing seek changes alter the rendered bytes.
	bandH := h / 8
	if bandH < 1 {
		bandH = 1
	}
	bandY := int(in.KeyframeSeq%int64(h-bandH+1))
	marker := hashMix(uint64(in.KeyframeMaster) ^ base)
	for y := bandY; y < bandY+bandH && y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(marker >> 40), G: uint8(marker >> 32),
				B: uint8(marker >> 24), A: 255,
			})
		}
	}
	return img
}

// writePNG encodes and writes an NRGBA image deterministically. imaging is
// used for the save path; PNG is a lossless format so hashes are stable.
func writePNG(path string, img *image.NRGBA) error {
	return imaging.Save(img, path, imaging.PNGCompressionLevel(png.BestCompression))
}

// pngBytes returns the canonical encoded bytes used for hashing. Encoding to
// a buffer (rather than re-reading the file) keeps the hash independent of
// filesystem metadata.
func pngBytes(img *image.NRGBA) []byte {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	_ = enc.Encode(&buf, img)
	return buf.Bytes()
}
