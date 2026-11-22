// Package render generates deterministic offscreen frames at fixed
// sample times. The same (Params, index) always yields byte-identical
// pixels, so frame SHA-256 digests are stable across runs.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
)

// Params describe the deterministic renderer.
type Params struct {
	Width  int
	Height int
	Seed   int64
}

// splitmix64 is a deterministic integer PRNG.
type splitmix64 uint64

func (s *splitmix64) next() uint64 {
	*s += 0x9E3779B97F4A7C15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xBF58476C1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (p Params) rng(index int) splitmix64 {
	return splitmix64(uint64(p.Seed)*0x2545F4914F6CDD1D ^ uint64(index)*0x165667B19E3779F9)
}

// reference returns the ideal noise-free pixel at (x, y).
func reference(x, y int) int {
	return (x + y) % 256
}

// Reference renders the ideal frame used as PSNR ground truth.
func Reference(p Params) []byte {
	buf := make([]byte, p.Width*p.Height)
	for y := 0; y < p.Height; y++ {
		for x := 0; x < p.Width; x++ {
			buf[y*p.Width+x] = byte(reference(x, y))
		}
	}
	return buf
}

// Frame renders the deterministic frame for a sample index: the
// reference pattern plus small deterministic noise (±4 LSB).
func Frame(p Params, index int) []byte {
	buf := make([]byte, p.Width*p.Height)
	rng := p.rng(index)
	for y := 0; y < p.Height; y++ {
		for x := 0; x < p.Width; x++ {
			noise := int(rng.next()&0x7) - 4 // -4..+3
			v := reference(x, y) + noise
			if v < 0 {
				v = 0
			}
			if v > 255 {
				v = 255
			}
			buf[y*p.Width+x] = byte(v)
		}
	}
	return buf
}

// SHA256 hex-encodes the frame digest.
func SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PSNR computes peak signal-to-noise ratio in dB against the reference.
// Floating point is used only here, for the final quality statistic —
// never for time calculations.
func PSNR(frame, ref []byte) float64 {
	if len(frame) != len(ref) || len(frame) == 0 {
		return 0
	}
	var sum float64
	for i := range frame {
		d := float64(frame[i]) - float64(ref[i])
		sum += d * d
	}
	mse := sum / float64(len(frame))
	if mse == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/mse)
}
