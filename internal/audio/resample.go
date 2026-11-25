// Package audio performs deterministic channel normalization, resampling and loudness control.
package audio

import (
	"math"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

// Buffer is interleaved floating-point audio.
type Buffer struct {
	SampleRate int       `json:"sample_rate"`
	Channels   int       `json:"channels"`
	Samples    []float64 `json:"-"`
}

// FrameCount returns the number of multi-channel frames.
func (b Buffer) FrameCount() int {
	if b.Channels <= 0 {
		return 0
	}
	return len(b.Samples) / b.Channels
}

// FrameTime maps a frame count to the shared rational time base.
func FrameTime(frames, rate int) (clock.Rational, error) {
	return clock.SamplesToTime(int64(frames), int64(rate))
}

// PCM16ToBuffer converts interleaved signed PCM16 samples to float.
func PCM16ToBuffer(samples []int16, rate, channels int) (Buffer, error) {
	if rate <= 0 || channels <= 0 || len(samples)%channels != 0 {
		return Buffer{}, fault.New(fault.InvalidArgument, "", -1, "invalid PCM buffer rate/channels/length")
	}
	out := make([]float64, len(samples))
	for i, v := range samples {
		if v == math.MinInt16 {
			out[i] = -1
		} else {
			out[i] = float64(v) / 32768
		}
	}
	return Buffer{SampleRate: rate, Channels: channels, Samples: out}, nil
}

// ToPCM16 quantizes and clamps the buffer.
func (b Buffer) ToPCM16() []int16 {
	out := make([]int16, len(b.Samples))
	for i, v := range b.Samples {
		out[i] = floatToPCM16(v)
	}
	return out
}

func floatToPCM16(v float64) int16 {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	if v >= 0 {
		return int16(math.Round(v * 32767))
	}
	return int16(math.Round(v * 32768))
}

// FixedStereoMatrix returns the normalized matrix for a canonical input order.
// 6-channel order is L, R, C, LFE, Ls, Rs. Rows are normalized to unit energy.
func FixedStereoMatrix(inChannels int) ([][]float64, error) {
	switch inChannels {
	case 1:
		return [][]float64{{1}, {1}}, nil
	case 2:
		return [][]float64{{1, 0}, {0, 1}}, nil
	case 6:
		// Raw left row [1,0,1/√2,0,1/√2,0] has squared norm 2; divide by √2.
		c := 0.5
		return [][]float64{
			{1 / math.Sqrt2, 0, c, 0, c, 0},
			{0, 1 / math.Sqrt2, c, 0, 0, c},
		}, nil
	default:
		return nil, fault.New(fault.InvalidArgument, "", -1, "no fixed stereo matrix for %d channels", inChannels)
	}
}

// Downmix applies the fixed normalized layout matrix.
func Downmix(in Buffer, layout string) (Buffer, error) {
	if layout != "stereo" {
		return Buffer{}, fault.New(fault.InvalidArgument, "", -1, "layout %q unsupported; want stereo", layout)
	}
	matrix, err := FixedStereoMatrix(in.Channels)
	if err != nil {
		return Buffer{}, err
	}
	outChannels := len(matrix)
	frames := in.FrameCount()
	out := make([]float64, frames*outChannels)
	for f := 0; f < frames; f++ {
		for o := 0; o < outChannels; o++ {
			var sum float64
			for c := 0; c < in.Channels; c++ {
				sum += matrix[o][c] * in.Samples[f*in.Channels+c]
			}
			out[f*outChannels+o] = sum
		}
	}
	return Buffer{SampleRate: in.SampleRate, Channels: outChannels, Samples: out}, nil
}

// Resample performs deterministic linear interpolation on every channel.
func Resample(in Buffer, outRate int) (Buffer, error) {
	if outRate <= 0 {
		return Buffer{}, fault.New(fault.InvalidArgument, "", -1, "output rate must be positive")
	}
	if in.SampleRate == outRate {
		cp := append([]float64(nil), in.Samples...)
		return Buffer{SampleRate: outRate, Channels: in.Channels, Samples: cp}, nil
	}
	inFrames := in.FrameCount()
	if inFrames == 0 {
		return Buffer{SampleRate: outRate, Channels: in.Channels}, nil
	}
	outFrames := int(math.Round(float64(inFrames) * float64(outRate) / float64(in.SampleRate)))
	out := make([]float64, outFrames*in.Channels)
	step := float64(in.SampleRate) / float64(outRate)
	for i := 0; i < outFrames; i++ {
		pos := float64(i) * step
		i0 := int(pos)
		frac := pos - float64(i0)
		i1 := i0 + 1
		if i1 >= inFrames {
			i1 = inFrames - 1
		}
		if i0 >= inFrames {
			i0 = inFrames - 1
		}
		for c := 0; c < in.Channels; c++ {
			a := in.Samples[i0*in.Channels+c]
			b := in.Samples[i1*in.Channels+c]
			out[i*in.Channels+c] = a*(1-frac) + b*frac
		}
	}
	return Buffer{SampleRate: outRate, Channels: in.Channels, Samples: out}, nil
}

// Clone returns a deep copy.
func (b Buffer) Clone() Buffer {
	return Buffer{SampleRate: b.SampleRate, Channels: b.Channels, Samples: append([]float64(nil), b.Samples...)}
}
