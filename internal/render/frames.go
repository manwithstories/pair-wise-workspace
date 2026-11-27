// Package render generates the offscreen review frame sequence: each frame
// shows the timecode, the audio envelope and event markers, and every PNG
// gets a SHA-256 so results are verifiable.
package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"

	"github.com/disintegration/imaging"

	"avcheck/internal/clock"
)

// Event is a timeline marker drawn into the frames.
type Event struct {
	Tick  clock.Tick
	Label string
}

// Spec controls frame generation.
type Spec struct {
	Width, Height int
	FPS           int
	Duration      clock.Tick
}

// Frame records one rendered frame and its digest.
type Frame struct {
	Index  int
	Tick   clock.Tick
	SHA256 string
}

var (
	bg     = color.RGBA{18, 20, 26, 255}
	barCol = color.RGBA{90, 200, 250, 255}
	evCol  = color.RGBA{250, 120, 90, 255}
	tcCol  = color.RGBA{230, 230, 120, 255}
)

// Render draws the whole sequence into dir as frame-0000.png … and writes a
// frames.sha256 manifest. stereo is interleaved 48kHz audio. The output is
// deterministic: identical inputs yield identical digests.
func Render(dir string, spec Spec, stereo []int16, events []Event) ([]Frame, error) {
	if spec.FPS <= 0 || spec.Width <= 0 || spec.Height <= 0 {
		return nil, fmt.Errorf("render: bad spec %+v", spec)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	n := int(clock.ToMillis(spec.Duration) * float64(spec.FPS) / 1000)
	frames := make([]Frame, 0, n)
	var manifest bytes.Buffer
	for i := 0; i < n; i++ {
		t0 := clock.FromSamples(int64(i*48000/spec.FPS), 48000)
		hi := drawFrame(spec, stereo, events, i, t0)
		// Downsample 2x supersampled drawing for the final frame.
		img := imaging.Resize(hi, spec.Width, spec.Height, imaging.Linear)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(buf.Bytes())
		name := fmt.Sprintf("frame-%04d.png", i)
		if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
			return nil, err
		}
		hexSum := hex.EncodeToString(sum[:])
		frames = append(frames, Frame{Index: i, Tick: t0, SHA256: hexSum})
		fmt.Fprintf(&manifest, "%s  %s\n", hexSum, name)
	}
	if err := os.WriteFile(filepath.Join(dir, "frames.sha256"), manifest.Bytes(), 0o644); err != nil {
		return nil, err
	}
	return frames, nil
}

// drawFrame renders one frame at 2x size: background, envelope bars, event
// markers and a binary timecode strip (no font dependency).
func drawFrame(spec Spec, stereo []int16, events []Event, index int, tick clock.Tick) *image.RGBA {
	w, h := spec.Width*2, spec.Height*2
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fill(img, img.Bounds(), bg)

	// Envelope: one bar per 4 pixels, peak of the audio window it covers.
	frame0 := index * 48000 / spec.FPS
	for x := 0; x < w; x += 4 {
		s0 := frame0 + x*48000/spec.FPS/w
		s1 := frame0 + (x+4)*48000/spec.FPS/w
		peak := peakAbs(stereo, s0, s1)
		barH := int(float64(h-30) * float64(peak) / 32768)
		fill(img, image.Rect(x, h-20-barH, x+3, h-20), barCol)
	}

	// Event markers at their exact horizontal position.
	for _, ev := range events {
		x := int(int64(ev.Tick) * int64(w) / int64(spec.Duration))
		if x >= 0 && x < w {
			fill(img, image.Rect(x, 0, x+2, h-20), evCol)
		}
	}

	// Timecode strip: 20 bits of the frame index as bright/dark cells.
	for b := 0; b < 20; b++ {
		c := color.RGBA{40, 40, 46, 255}
		if index>>b&1 == 1 {
			c = tcCol
		}
		fill(img, image.Rect(4+b*8, h-16, 4+b*8+6, h-4), c)
	}
	return img
}

func peakAbs(stereo []int16, from, to int) int {
	if from < 0 {
		from = 0
	}
	if to > len(stereo)/2 {
		to = len(stereo) / 2
	}
	peak := 0
	for i := from; i < to; i++ {
		for c := 0; c < 2; c++ {
			v := int(stereo[i*2+c])
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
	}
	return peak
}

func fill(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	r = r.Intersect(img.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}
