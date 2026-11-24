// Package dsp performs deterministic audio conversion, R128 measurement and
// loudness normalization.
package dsp

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/clearliu/mediq/internal/core"
)

// Buffer is interleaved floating-point audio in [-1,1].
type Buffer struct {
	Rate        int
	Channels    int
	Interleaved []float64
}

// Frames returns the number of multi-channel sample frames.
func (b *Buffer) Frames() int {
	if b.Channels <= 0 {
		return 0
	}
	return len(b.Interleaved) / b.Channels
}

// Clone returns a copy that callers may mutate.
func (b *Buffer) Clone() *Buffer {
	pcm := make([]float64, len(b.Interleaved))
	copy(pcm, b.Interleaved)
	return &Buffer{Rate: b.Rate, Channels: b.Channels, Interleaved: pcm}
}

// DecodeL16 decodes big-endian signed L16 RTP payload.
func DecodeL16(payload []byte, channels int) (*Buffer, error) {
	if channels <= 0 || channels > 2 {
		return nil, errors.New("dsp: only mono and stereo L16 are supported")
	}
	if len(payload)%2 != 0 {
		return nil, errors.New("dsp: L16 payload has odd byte length")
	}
	samples := len(payload) / 2
	if samples%channels != 0 {
		return nil, errors.New("dsp: L16 sample count is not divisible by channels")
	}
	out := make([]float64, samples)
	for i := 0; i < samples; i++ {
		v := int16(binary.BigEndian.Uint16(payload[2*i : 2*i+2]))
		out[i] = float64(v) / 32768.0
	}
	return &Buffer{Rate: 0, Channels: channels, Interleaved: out}, nil
}

// NormalizeStereo duplicates mono and drops channels beyond the first pair.
func NormalizeStereo(in *Buffer) *Buffer {
	if in.Channels == core.OutputChannels {
		return in.Clone()
	}
	frames := in.Frames()
	out := &Buffer{Rate: in.Rate, Channels: core.OutputChannels, Interleaved: make([]float64, frames*2)}
	if in.Channels == 1 {
		for i, v := range in.Interleaved {
			out.Interleaved[2*i] = v
			out.Interleaved[2*i+1] = v
		}
		return out
	}
	for f := 0; f < frames; f++ {
		out.Interleaved[2*f] = in.Interleaved[f*in.Channels]
		out.Interleaved[2*f+1] = in.Interleaved[f*in.Channels+1]
	}
	return out
}

// Resample converts 44.1 or 48 kHz audio to 48 kHz. The 44.1 path uses a
// windowed-sinc rational conversion with edge replication.
func Resample(in *Buffer, outRate int) *Buffer {
	if in.Rate == outRate || in.Rate <= 0 {
		out := in.Clone()
		out.Rate = outRate
		return out
	}
	frames := in.Frames()
	outFrames := int(math.Round(float64(frames) * float64(outRate) / float64(in.Rate)))
	out := &Buffer{Rate: outRate, Channels: in.Channels, Interleaved: make([]float64, outFrames*in.Channels)}

	if in.Rate == 44100 && outRate == 48000 {
		resample441To48(in, out, outFrames)
		return out
	}
	resampleLinear(in, out, outFrames)
	return out
}

func resampleLinear(in *Buffer, out *Buffer, outFrames int) {
	frames := in.Frames()
	ratio := float64(in.Rate) / float64(out.Rate)
	for n := 0; n < outFrames; n++ {
		pos := float64(n) * ratio
		i := int(math.Floor(pos))
		frac := pos - float64(i)
		i0 := clampIndex(i, frames)
		i1 := clampIndex(i+1, frames)
		for ch := 0; ch < in.Channels; ch++ {
			a := in.Interleaved[i0*in.Channels+ch]
			b := in.Interleaved[i1*in.Channels+ch]
			out.Interleaved[n*in.Channels+ch] = a+(b-a)*frac
		}
	}
}

func resample441To48(in *Buffer, out *Buffer, outFrames int) {
	const radius = 12
	ratio := 147.0 / 160.0 // input position per output sample
	frames := in.Frames()
	for n := 0; n < outFrames; n++ {
		pos := float64(n) * ratio
		center := int(math.Floor(pos + 0.5))
		for ch := 0; ch < in.Channels; ch++ {
			var sum, weight float64
			for k := center - radius; k <= center+radius; k++ {
				t := pos - float64(k)
				if math.Abs(t) > radius {
					continue
				}
				w := sinc(t) * hann(0.5*(1-math.Abs(t)/radius))
				idx := clampIndex(k, frames)
				sum += w * in.Interleaved[idx*in.Channels+ch]
				weight += w
			}
			if weight != 0 {
				sum /= weight
			}
			out.Interleaved[n*in.Channels+ch] = clampFloat(sum, -1, 1)
		}
	}
}

// ApplyDB applies a gain specified in decibels.
func ApplyDB(b *Buffer, db float64) {
	gain := math.Pow(10, db/20)
	for i := range b.Interleaved {
		b.Interleaved[i] = clampFloat(b.Interleaved[i]*gain, -1, 1)
	}
}

// EncodeS16LE encodes interleaved PCM as signed 16-bit little-endian bytes.
func EncodeS16LE(b *Buffer) []byte {
	buf := make([]byte, len(b.Interleaved)*2)
	for i, v := range b.Interleaved {
		s := int16(math.Round(clampFloat(v, -1, 1) * 32767))
		binary.LittleEndian.PutUint16(buf[2*i:2*i+2], uint16(s))
	}
	return buf
}

// DecodeS16LE decodes signed 16-bit little-endian bytes.
func DecodeS16LE(pcm []byte, rate, channels int) *Buffer {
	out := make([]float64, len(pcm)/2)
	for i := range out {
		v := int16(binary.LittleEndian.Uint16(pcm[2*i : 2*i+2]))
		out[i] = float64(v) / 32768.0
	}
	return &Buffer{Rate: rate, Channels: channels, Interleaved: out}
}

func clampIndex(i, n int) int {
	if n <= 0 {
		return 0
	}
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sinc(x float64) float64 {
	if math.Abs(x) < 1e-12 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

func hann(x float64) float64 {
	if x <= 0 || x >= 1 {
		return 0
	}
	return 0.5 - 0.5*math.Cos(2*math.Pi*x)
}
