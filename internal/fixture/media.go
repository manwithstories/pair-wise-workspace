package fixture

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"mediad/internal/manifest"
	"mediad/internal/stream"
)

// frameW and frameH are the source frame dimensions; proxies downscale.
const (
	frameW = 640
	frameH = 360
)

// frameImage renders a deterministic synthetic frame: a gradient, a
// moving block and mild noise, all derived from (seed, seq).
func frameImage(seed, seq int64) *image.RGBA {
	rng := rand.New(rand.NewSource(seed*7919 + seq))
	img := image.NewRGBA(image.Rect(0, 0, frameW, frameH))
	for y := 0; y < frameH; y++ {
		for x := 0; x < frameW; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 255 / frameW), G: uint8(y * 255 / frameH), B: 128, A: 255})
		}
	}
	bx := int(seq*5) % (frameW - 40)
	for y := frameH/2 - 30; y < frameH/2+30; y++ {
		for x := bx; x < bx+40; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 220, G: 150, B: 150, A: 255})
		}
	}
	// blocky mid-frequency texture survives the proxy downscale and gives
	// the encoder a realistic byte budget to spend
	const bs = 3
	for by := 0; by < frameH/bs; by++ {
		for bx := 0; bx < frameW/bs; bx++ {
			d := int8(rng.Intn(21) - 10)
			for y := by * bs; y < by*bs+bs; y++ {
				for x := bx * bs; x < bx*bs+bs; x++ {
					c := img.RGBAAt(x, y)
					img.SetRGBA(x, y, color.RGBA{
						R: clampSigned(c.R, d), G: clampSigned(c.G, d), B: clampSigned(c.B, d), A: 255})
				}
			}
		}
	}
	return img
}

func clampSigned(v uint8, d int8) uint8 {
	s := int(v) + int(d)
	if s < 0 {
		return 0
	}
	if s > 255 {
		return 255
	}
	return uint8(s)
}

func frameName(codec string, seq int64) string {
	ext := "png"
	if codec == "jpeg" {
		ext = "jpg"
	}
	return fmt.Sprintf("%06d.%s", seq, ext)
}

func encodeFrame(codec string, img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	var err error
	if codec == "jpeg" {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(&buf, img)
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func shaOf(data []byte) string { return stream.SHA256Hex(data) }

// audioTrack writes the case PCM file and returns its manifest track plus
// the expected 48 kHz stereo output frame count (0 when the track is
// intentionally unsupported).
func audioTrack(dir, name string, frames int) (manifest.Track, int64, error) {
	rel := filepath.Join("a1", "audio.pcm")
	at := manifest.Track{ID: "a1", Kind: "audio", Format: "s16le", File: rel}
	var data []byte
	var outFrames int64
	switch name {
	case "success":
		at.Layout, at.SampleRate = "stereo", 48000
		n := int64(frames) * 48000 / FPS
		data = pcmSine(n, 48000, 2)
		outFrames = n
	case "corrupt":
		at.Layout, at.SampleRate = "mono", 44100
		n := int64(frames) * 44100 / FPS
		data = pcmSine(n, 44100, 1)
		outFrames = int64(frames) * 48000 / FPS
	default: // edge: unsupported layout, content irrelevant
		at.Layout, at.SampleRate = "7.1", 48000
		data = pcmSine(4800, 48000, 8)
	}
	if err := os.WriteFile(filepath.Join(dir, rel), data, 0o644); err != nil {
		return at, 0, err
	}
	at.SHA256 = shaOf(data)
	at.Bytes = int64(len(data))
	return at, outFrames, nil
}

// pcmSine renders a 440 Hz sine wave as interleaved s16le samples.
func pcmSine(samples int64, rate, channels int) []byte {
	buf := make([]byte, 0, samples*int64(channels)*2)
	for i := int64(0); i < samples; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		for c := 0; c < channels; c++ {
			buf = append(buf, byte(v), byte(v>>8))
		}
	}
	return buf
}
