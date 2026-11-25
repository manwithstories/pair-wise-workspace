package fixture

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
)

// FrameImage renders deterministic RGB content for global frame index i. The
// pattern encodes the index so a decoded PNG can be traced back to its source
// frame without any side table.
func FrameImage(i, w, h int) *image.RGBA {
	return seededImage(0, i, w, h)
}

// seededImage varies the frame pattern with seed without harming JPEG validity.
func seededImage(seed int64, i, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Speed and tint shift with the seed; identical (seed, i) always yields
	// identical pixels.
	speed := 3 + int(seed%5)
	tint := uint8(seed % 251)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// A moving bar plus a per-frame tint keeps JPEG payloads distinct.
			bar := (x + i*speed) % w
			var c color.RGBA
			if bar < w/8 {
				c = color.RGBA{R: 255, G: uint8((i*7 + int(seed)) % 256), B: tint, A: 255}
			} else {
				c = color.RGBA{
					R: uint8((x*2 + i + int(seed)) % 256),
					G: uint8((y*3 + i*5) % 256),
					B: uint8((x ^ y) % 256),
					A: 255,
				}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// JPEGFrame encodes frame i at quality 80. The JPEG encoder is deterministic
// for identical input, so repeated generation yields identical bytes.
func JPEGFrame(i, w, h int) ([]byte, error) {
	return seededJPEG(0, i, w, h)
}

func seededJPEG(seed int64, i, w, h int) ([]byte, error) {
	img := seededImage(seed, i, w, h)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, fmt.Errorf("fixture: encode frame %d: %w", i, err)
	}
	return buf.Bytes(), nil
}

// Frames returns the JPEG payloads of frames [0,n).
func Frames(n, w, h int) ([][]byte, error) {
	return FramesWithSeed(0, n, w, h)
}

// FramesWithSeed returns seed-dependent JPEG payloads of frames [0,n).
func FramesWithSeed(seed int64, n, w, h int) ([][]byte, error) {
	out := make([][]byte, n)
	for i := range out {
		b, err := seededJPEG(seed, i, w, h)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

// KeyFlag reports whether global frame i is a keyframe in the MP4 track.
func KeyFlag(i int) bool { return i%MP4KeyInterval == 0 }
