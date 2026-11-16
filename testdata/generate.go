// Package fixture generates deterministic multi-channel WAV fixtures:
// stereo sines, low-frequency noise, transient clipping, silence
// segments and multichannel files with distinct per-channel gains.
// All content is derived from a seed; no external files are used.
package fixture

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"loudcheck/internal/audio"
	"loudcheck/internal/filter"
	"loudcheck/internal/gate"
)

// Options controls fixture generation.
type Options struct {
	OutDir   string
	Seed     int64
	Channels int // channel count of the multichannel fixture (3..8, default 6)
}

const sampleRate = 48000

// prng is a deterministic xorshift64* source of uniform [-1,1) values.
type prng struct{ s uint64 }

func (p *prng) next() float64 {
	p.s ^= p.s << 13
	p.s ^= p.s >> 7
	p.s ^= p.s << 17
	p.s *= 2685821657736338717
	return float64(p.s>>11)/float64(uint64(1)<<53)*2 - 1
}

// kUnitEnergy measures the K-weighted mean square of a unit-amplitude
// sine at freq, used to calibrate fixture amplitudes analytically.
func kUnitEnergy(freq float64) float64 {
	kw, err := filter.NewKWeight(sampleRate)
	if err != nil {
		panic(err)
	}
	var sum float64
	var n int
	for i := 0; i < 9600; i++ {
		y := kw.Process(math.Sin(2 * math.Pi * freq * float64(i) / sampleRate))
		if i >= 4800 { // skip filter warmup
			sum += y * y
			n++
		}
	}
	return sum / float64(n)
}

// amplitudeFor returns the sine peak amplitude that yields targetLUFS
// when the per-channel gains contribute weighted energy w.
func amplitudeFor(targetLUFS, unitE, w float64) float64 {
	return math.Sqrt(math.Pow(10, (targetLUFS-gate.OffsetLU)/10) / (unitE * w))
}

// Generate writes the fixture set and returns the file paths.
func Generate(opts Options) ([]string, error) {
	if opts.Channels == 0 {
		opts.Channels = 6
	}
	if opts.Channels < 3 || opts.Channels > audio.MaxChannels {
		return nil, fmt.Errorf("fixture: channels %d out of range 3..%d", opts.Channels, audio.MaxChannels)
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	rnd := &prng{s: uint64(opts.Seed)*2654435761 + 1}
	unitE := kUnitEnergy(1000)

	files := []struct {
		name string
		gen  func() ([]float64, int, int, bool) // samples, channels, bits, floatFmt
	}{
		{"sine_stereo_16.wav", func() ([]float64, int, int, bool) {
			return sine(4, amplitudeFor(-23, unitE, 2), nil, nil), 2, 16, false
		}},
		{"sine_lownoise_24.wav", func() ([]float64, int, int, bool) {
			noise := lfNoise(rnd, 4*sampleRate, 0.02)
			return sine(4, amplitudeFor(-30, unitE, 2), nil, noise), 2, 24, false
		}},
		{"silence_loud_16.wav", func() ([]float64, int, int, bool) {
			return silenceSine(1.5, 2.5, 1.0, amplitudeFor(-16, unitE, 2)), 2, 16, false
		}},
		{"multich_float.wav", func() ([]float64, int, int, bool) {
			gains := channelGains(rnd, opts.Channels)
			layout := audio.LayoutForChannels(opts.Channels)
			w, _ := audio.Weights(layout, opts.Channels)
			var wsum float64
			for c, g := range gains {
				wsum += w[c] * g * g
			}
			return sine(4, amplitudeFor(-23, unitE, wsum), gains, nil), opts.Channels, 32, true
		}},
		{"transient_clip_16.wav", func() ([]float64, int, int, bool) {
			s := sine(4, amplitudeFor(-23, unitE, 2), nil, nil)
			for i := 2 * sampleRate; i < 2*sampleRate+3; i++ { // 3-sample burst
				v := 0.999
				if i%2 == 1 {
					v = -v
				}
				s[i*2], s[i*2+1] = v, v
			}
			return s, 2, 16, false
		}},
	}

	var paths []string
	for _, f := range files {
		samples, ch, bits, flt := f.gen()
		p := filepath.Join(opts.OutDir, f.name)
		if err := writeWAV(p, sampleRate, ch, bits, flt, samples); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// sine renders dur seconds of a 1 kHz sine at peak amplitude amp into
// interleaved channels, scaled by gains (nil = unity) and summed with
// an optional additive mono noise signal.
func sine(dur, amp float64, gains []float64, noise []float64) []float64 {
	ch := 2
	if gains != nil {
		ch = len(gains)
	}
	n := int(dur * sampleRate)
	out := make([]float64, n*ch)
	for i := 0; i < n; i++ {
		v := amp * math.Sin(2*math.Pi*1000*float64(i)/sampleRate)
		if noise != nil {
			v += noise[i]
		}
		for c := 0; c < ch; c++ {
			g := 1.0
			if gains != nil {
				g = gains[c]
			}
			out[i*ch+c] = v * g
		}
	}
	return out
}

// silenceSine renders pre seconds of silence, dur seconds of stereo
// sine, then post seconds of silence.
func silenceSine(pre, dur, post, amp float64) []float64 {
	nPre, nDur, nPost := int(pre*sampleRate), int(dur*sampleRate), int(post*sampleRate)
	out := make([]float64, (nPre+nDur+nPost)*2)
	for i := 0; i < nDur; i++ {
		v := amp * math.Sin(2*math.Pi*1000*float64(i)/sampleRate)
		out[(nPre+i)*2] = v
		out[(nPre+i)*2+1] = v
	}
	return out
}

// lfNoise renders n samples of low-frequency noise (leaky integrator
// driven by white noise) scaled to the given RMS.
func lfNoise(rnd *prng, n int, rms float64) []float64 {
	out := make([]float64, n)
	var lp, sum float64
	for i := range out {
		lp += 0.02 * (rnd.next() - lp)
		out[i] = lp
		sum += lp * lp
	}
	scale := rms / math.Sqrt(sum/float64(n))
	for i := range out {
		out[i] *= scale
	}
	return out
}

// channelGains returns distinct per-channel gains with a small
// deterministic seed-dependent jitter.
func channelGains(rnd *prng, channels int) []float64 {
	base := []float64{1.0, 0.8, 0.6, 0.5, 0.9, 0.7, 0.85, 0.75}
	g := make([]float64, channels)
	for c := range g {
		g[c] = base[c] + 0.05*rnd.next()
	}
	return g
}

// writeWAV writes interleaved samples as a RIFF/WAVE file.
func writeWAV(path string, rate, channels, bits int, floatFmt bool, samples []float64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	blockAlign := channels * (bits / 8)
	dataSize := len(samples) * (bits / 8)
	enc := uint16(1)
	if floatFmt {
		enc = 3
	}
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+dataSize))
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], enc)
	binary.LittleEndian.PutUint16(hdr[22:], uint16(channels))
	binary.LittleEndian.PutUint32(hdr[24:], uint32(rate))
	binary.LittleEndian.PutUint32(hdr[28:], uint32(rate*blockAlign))
	binary.LittleEndian.PutUint16(hdr[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:], uint16(bits))
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(dataSize))
	if _, err := f.Write(hdr); err != nil {
		return err
	}

	buf := make([]byte, bits/8)
	for _, s := range samples {
		switch {
		case floatFmt:
			binary.LittleEndian.PutUint32(buf, math.Float32bits(float32(s)))
		case bits == 16:
			v := int16(math.Round(math.Max(-1, math.Min(1-1.0/32768, s)) * 32768))
			binary.LittleEndian.PutUint16(buf, uint16(v))
		case bits == 24:
			v := int32(math.Round(math.Max(-1, math.Min(1-1.0/8388608, s)) * 8388608))
			buf[0], buf[1], buf[2] = byte(v), byte(v>>8), byte(v>>16)
		}
		if _, err := f.Write(buf); err != nil {
			return err
		}
	}
	return nil
}
