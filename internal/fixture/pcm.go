// Package fixture generates deterministic Matroska/WebM sample archives and
// RTP packet streams for the archivecheck demo.  Every byte is derived from
// the seed; no external media is read.
package fixture

import (
	"encoding/binary"
)

// RawAudio is a 16-bit little-endian PCM sample generator.  Keeping phase
// across blocks makes concatenated blocks continuous while output stays
// bit-identical for a given seed.
type RawAudio struct {
	SampleRate uint32
	Channels   uint8

	// Sine is the partial state that keeps phase continuous across blocks.
	Phase float64
	Freq  Hz
}

// Hz is a frequency in hertz.
type Hz float64

func (r *RawAudio) Next(frames int) []int16 {
	out := make([]int16, frames*int(r.Channels))
	step := 2 * 3.14159265358979323846 * float64(r.Freq) / float64(r.SampleRate)
	for i := 0; i < frames; i++ {
		r.Phase += step
		if r.Phase > 6.283185307179586 {
			r.Phase -= 6.283185307179586
		}
		v := int16(12000 * sin(r.Phase))
		for c := 0; c < int(r.Channels); c++ {
			out[i*int(r.Channels)+c] = v
		}
	}
	return out
}

// EncodePCM returns the raw little-endian bytes of samples.
func EncodePCM(samples []int16) []byte {
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return buf
}

// sin is a small deterministic sine approximation, avoiding libm so output
// is bit-identical across architectures.
func sin(x float64) float64 {
	// Range-reduce to [-pi,pi].
	const twoPi = 6.283185307179586
	for x > 3.141592653589793 {
		x -= twoPi
	}
	for x < -3.141592653589793 {
		x += twoPi
	}
	// Taylor series around 0 is accurate to ~1e-12 for |x|<=pi at 11 terms.
	x2 := x * x
	term := x
	sum := x
	for n := 1; n <= 10; n++ {
		term *= -x2 / float64((2*n)*(2*n+1))
		sum += term
	}
	return sum
}
