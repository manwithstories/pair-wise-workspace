// Package source generates reproducible synthetic YUV420p sequences.
package source

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
)

// Config describes a synthetic sequence.
type Config struct {
	Seed   int64
	Frames int
	Width  int
	Height int
}

// Validate checks generator parameters.
func (c Config) Validate() error {
	if c.Width < 16 || c.Height < 16 || c.Width%2 != 0 || c.Height%2 != 0 {
		return fmt.Errorf("width/height must be even and >= 16, got %dx%d", c.Width, c.Height)
	}
	if c.Frames < 8 {
		return fmt.Errorf("frames must be >= 8 (need cut plus two noise segments), got %d", c.Frames)
	}
	return nil
}

// FrameSize returns bytes per YUV420p frame.
func FrameSize(w, h int) int { return w * h * 3 / 2 }

// Layout returns cut frame and the two noise ranges [start,end).
func Layout(frames int) (cut int, noiseA, noiseB [2]int) {
	cut = frames / 2
	nlen := frames / 12
	if nlen < 2 {
		nlen = 2
	}
	noiseA = [2]int{frames / 4, frames/4 + nlen}
	noiseB = [2]int{3 * frames / 4, 3*frames/4 + nlen}
	return
}

func inRange(i int, r [2]int) bool { return i >= r[0] && i < r[1] }

// triangle wave in [0,255] with given period, smooth panning gradient.
func tri(pos, period int) int {
	m := pos % period
	if m < 0 {
		m += period
	}
	half := period / 2
	if m < half {
		return m * 255 / half
	}
	return (period - m) * 255 / half
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// RenderFrame writes one frame (Y then U then V planes) into buf.
func (c Config) RenderFrame(buf []byte, index int) {
	w, h := c.Width, c.Height
	cut, noiseA, noiseB := Layout(c.Frames)
	sceneB := index >= cut
	y := buf[:w*h]
	u := buf[w*h : w*h+w*h/4]
	v := buf[w*h+w*h/4 : FrameSize(w, h)]

	panSpeed := 2
	lumaBase := 24
	period := w * 2
	if sceneB {
		panSpeed = -3
		lumaBase = 60
	}
	for row := 0; row < h; row++ {
		vg := row * 40 / h // gentle vertical gradient
		for col := 0; col < w; col++ {
			var lum int
			if !sceneB {
				lum = lumaBase + tri(col+panSpeed*index, period)*160/256 + vg
			} else {
				lum = lumaBase + tri(col+row+panSpeed*index, period)*120/256 + (h-row)*30/h
			}
			// fine texture present in every frame: consumes bits uniformly
			lum += tri(col*3+row*2+index, 32)*12/256 - 6
			y[row*w+col] = clamp8(lum)
		}
	}
	// light global grain on every frame
	grng := rand.New(rand.NewSource(c.Seed*31 + int64(index)*2654435761))
	for i := range y {
		y[i] = clamp8(int(y[i]) + grng.Intn(3) - 1)
	}
	if inRange(index, noiseA) || inRange(index, noiseB) {
		rng := rand.New(rand.NewSource(c.Seed*7919 + int64(index)*104729))
		for i := range y {
			y[i] = clamp8(int(y[i]) + rng.Intn(9) - 4)
		}
		// sparse grain speckles
		specks := w * h / 400
		for s := 0; s < specks; s++ {
			p := rng.Intn(w * h)
			y[p] = clamp8(int(y[p]) + rng.Intn(31) - 15)
		}
	}
	cw, ch := w/2, h/2
	for row := 0; row < ch; row++ {
		for col := 0; col < cw; col++ {
			phase := col*64/cw + index
			if sceneB {
				phase = row*64/ch + index*2
			}
			u[row*cw+col] = clamp8(128 + tri(phase, cw)*40/256 - 20)
			v[row*cw+col] = clamp8(128 + tri(row+col+index, cw+ch)*40/256 - 20)
		}
	}
}

// Generate streams the sequence to w and returns its SHA-256 hex digest.
func Generate(w io.Writer, cfg Config) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	h := sha256.New()
	bw := bufio.NewWriterSize(io.MultiWriter(w, h), 1<<20)
	frame := make([]byte, FrameSize(cfg.Width, cfg.Height))
	for i := 0; i < cfg.Frames; i++ {
		cfg.RenderFrame(frame, i)
		if _, err := bw.Write(frame); err != nil {
			return "", fmt.Errorf("write frame %d: %w", i, err)
		}
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// GenerateFile creates path and writes the sequence, returning the digest.
func GenerateFile(path string, cfg Config) (string, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return Generate(f, cfg)
}
