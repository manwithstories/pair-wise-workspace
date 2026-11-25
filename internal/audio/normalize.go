// Package audio converts PCM layouts and applies deterministic loudness and
// true-peak normalization.
package audio

import (
	"encoding/binary"
	"math"
)

// TargetRate and TargetChannels are the demo kernel's delivery layout.
const (
	TargetRate     = 48000
	TargetChannels = 2
)

// Buffer is deinterleaved-by-convention interleaved float PCM in [-1,1].
type Buffer struct {
	Rate     int
	Channels int
	Samples  []float64
}

// Frames returns the number of interleaved sample frames.
func (b *Buffer) Frames() int {
	if b.Channels <= 0 {
		return 0
	}
	return len(b.Samples) / b.Channels
}

// FromS16 converts interleaved signed 16-bit PCM to float64.
func FromS16(data []byte, rate, channels int) *Buffer {
	n := len(data) / 2
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(data[2*i:]))
		out[i] = float64(v) / 32768.0
	}
	return &Buffer{Rate: rate, Channels: channels, Samples: out}
}

// ToS16 clips deterministically and encodes little-endian signed 16-bit PCM.
func (b *Buffer) ToS16() []byte {
	out := make([]byte, 2*len(b.Samples))
	for i, x := range b.Samples {
		if x > 1 {
			x = 1
		} else if x < -1 {
			x = -1
		}
		v := int64(math.Round(x * 32767.0))
		if v > math.MaxInt16 {
			v = math.MaxInt16
		} else if v < math.MinInt16 {
			v = math.MinInt16
		}
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(v)))
	}
	return out
}

// ConvertFormat resamples linearly and changes channel layout. Linear
// interpolation is intentionally dependency-free and deterministic.
func ConvertFormat(in *Buffer, outRate, outChannels int) *Buffer {
	if outRate <= 0 {
		outRate = TargetRate
	}
	if outChannels <= 0 {
		outChannels = TargetChannels
	}
	if in == nil || in.Frames() == 0 {
		return &Buffer{Rate: outRate, Channels: outChannels}
	}
	resampled := resampleLinear(in, outRate)
	return remixChannels(resampled, outChannels)
}

func resampleLinear(in *Buffer, outRate int) *Buffer {
	if in.Rate == outRate {
		return &Buffer{Rate: in.Rate, Channels: in.Channels, Samples: append([]float64(nil), in.Samples...)}
	}
	inFrames := in.Frames()
	outFrames := (int64(inFrames)*int64(outRate) + int64(in.Rate)/2) / int64(in.Rate)
	if outFrames < 1 && inFrames > 0 {
		outFrames = 1
	}
	out := make([]float64, int(outFrames)*in.Channels)
	step := float64(in.Rate) / float64(outRate)
	for f := int64(0); f < outFrames; f++ {
		pos := float64(f) * step
		i0 := int(pos)
		frac := pos - float64(i0)
		i1 := i0 + 1
		if i0 >= inFrames-1 {
			i0, i1 = inFrames-1, inFrames-1
			frac = 0
		} else if i1 >= inFrames {
			i1 = inFrames - 1
		}
		for c := 0; c < in.Channels; c++ {
			a := in.Samples[i0*in.Channels+c]
			b := in.Samples[i1*in.Channels+c]
			out[int(f)*in.Channels+c] = a + (b-a)*frac
		}
	}
	return &Buffer{Rate: outRate, Channels: in.Channels, Samples: out}
}

func remixChannels(in *Buffer, outChannels int) *Buffer {
	if in.Channels == outChannels {
		return in
	}
	frames := in.Frames()
	out := make([]float64, frames*outChannels)
	for f := 0; f < frames; f++ {
		switch {
		case in.Channels == 1:
			v := in.Samples[f]
			for c := 0; c < outChannels; c++ {
				out[f*outChannels+c] = v
			}
		case outChannels == 1:
			sum := 0.0
			for c := 0; c < in.Channels; c++ {
				sum += in.Samples[f*in.Channels+c]
			}
			out[f] = sum / float64(in.Channels)
		default:
			for c := 0; c < outChannels; c++ {
				src := c
				if src >= in.Channels {
					src = in.Channels - 1
				}
				out[f*outChannels+c] = in.Samples[f*in.Channels+src]
			}
		}
	}
	return &Buffer{Rate: in.Rate, Channels: outChannels, Samples: out}
}

// CountClippedS16 reports samples at either signed full-scale endpoint.
func CountClippedS16(data []byte) int {
	count := 0
	for i := 0; i+1 < len(data); i += 2 {
		v := int16(binary.LittleEndian.Uint16(data[i:]))
		if v == math.MaxInt16 || v == -32768 {
			count++
		}
	}
	return count
}

// DBFSToAmplitude converts a dBFS value to linear amplitude.
func DBFSToAmplitude(db float64) float64 { return math.Pow(10, db/20) }

// AmplitudeToDBFS converts linear amplitude to dBFS, flooring silence.
func AmplitudeToDBFS(a float64) float64 {
	if a <= 1e-12 {
		return -120
	}
	return 20 * math.Log10(a)
}
