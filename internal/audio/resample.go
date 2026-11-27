// Package audio normalizes decoded audio to the media core target format:
// 48 kHz, stereo, then measures and applies loudness normalization.
//
// The DSP is implemented in Go rather than delegated to libswresample because
// go-astiav v0.42.0 does not expose the swr option/init entry points. Pure Go
// also makes every stage deterministic and directly testable.
package audio

import (
	"fmt"
	"math"
)

// TargetRate is the normalized sample rate.
const TargetRate = 48000

// Buffer is de-interleaved float audio. Planes holds one slice per channel;
// all channels have the same length (Frames samples).
type Buffer struct {
	Rate   int
	Planes [][]float32
	Layout string
}

// Channels returns the channel count.
func (b Buffer) Channels() int { return len(b.Planes) }

// Frames returns the number of samples per channel.
func (b Buffer) Frames() int {
	if len(b.Planes) == 0 {
		return 0
	}
	return len(b.Planes[0])
}

// Validate checks buffer consistency.
func (b Buffer) Validate() error {
	if b.Rate <= 0 || b.Rate > 768000 {
		return fmt.Errorf("audio: invalid sample rate %d", b.Rate)
	}
	if len(b.Planes) == 0 || len(b.Planes) > 8 {
		return fmt.Errorf("audio: invalid channel count %d", len(b.Planes))
	}
	n := len(b.Planes[0])
	for i, p := range b.Planes {
		if len(p) != n {
			return fmt.Errorf("audio: channel %d has %d frames, want %d", i, len(p), n)
		}
	}
	return nil
}

// Interleave copies planar float data into one planar slice per channel from a
// decoder buffer produced by astiav's SamplesCopyToBuffer (planes contiguous).
func Deinterleave(buf []byte, channels, frames int, format string) (Buffer, error) {
	if channels <= 0 || channels > 8 {
		return Buffer{}, fmt.Errorf("audio: invalid channel count %d", channels)
	}
	if frames < 0 {
		return Buffer{}, fmt.Errorf("audio: negative frame count %d", frames)
	}
	b := Buffer{Planes: make([][]float32, channels)}
	for c := range b.Planes {
		b.Planes[c] = make([]float32, frames)
	}
	switch format {
	case "fltp":
		need := channels * frames * 4
		if len(buf) < need {
			return Buffer{}, fmt.Errorf("audio: fltp buffer %d < %d", len(buf), need)
		}
		for c := 0; c < channels; c++ {
			for i := 0; i < frames; i++ {
				off := (c*frames + i) * 4
				b.Planes[c][i] = math.Float32frombits(le32(buf[off:]))
			}
		}
	case "flt":
		need := channels * frames * 4
		if len(buf) < need {
			return Buffer{}, fmt.Errorf("audio: flt buffer %d < %d", len(buf), need)
		}
		for i := 0; i < frames; i++ {
			for c := 0; c < channels; c++ {
				off := (i*channels + c) * 4
				b.Planes[c][i] = math.Float32frombits(le32(buf[off:]))
			}
		}
	case "s16p":
		need := channels * frames * 2
		if len(buf) < need {
			return Buffer{}, fmt.Errorf("audio: s16p buffer %d < %d", len(buf), need)
		}
		for c := 0; c < channels; c++ {
			for i := 0; i < frames; i++ {
				off := (c*frames + i) * 2
				b.Planes[c][i] = float32(int16(le16(buf[off:]))) / 32768
			}
		}
	case "s16":
		need := channels * frames * 2
		if len(buf) < need {
			return Buffer{}, fmt.Errorf("audio: s16 buffer %d < %d", len(buf), need)
		}
		for i := 0; i < frames; i++ {
			for c := 0; c < channels; c++ {
				off := (i*channels + c) * 2
				b.Planes[c][i] = float32(int16(le16(buf[off:]))) / 32768
			}
		}
	default:
		return Buffer{}, fmt.Errorf("audio: unsupported sample format %q", format)
	}
	b.Rate = 0 // caller sets rate; kept explicit to avoid a guessed value
	return b, nil
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// Normalizer resamples and downmixes to 48 kHz stereo.
type Normalizer struct {
	res     *Resampler
	mapping [][2]float64 // per input channel: left, right gain
	enabled bool
	maxGain float64
}

// NewNormalizer prepares normalization for an input layout.
func NewNormalizer(rate, channels int, layout string) (*Normalizer, error) {
	if rate <= 0 || rate > 768000 {
		return nil, fmt.Errorf("audio: invalid rate %d", rate)
	}
	if channels <= 0 || channels > 8 {
		return nil, fmt.Errorf("audio: invalid channels %d", channels)
	}
	m, err := DownmixMatrix(channels, layout)
	if err != nil {
		return nil, err
	}
	return &Normalizer{
		res:     NewResampler(rate, TargetRate),
		mapping: m,
		enabled: true,
	}, nil
}

// Enabled reports whether normalization runs; disabled normalizers pass
// 48 kHz stereo buffers through untouched.
func (n *Normalizer) Enabled() bool { return n != nil && n.enabled }

// Process consumes one input buffer and returns normalized stereo output.
func (n *Normalizer) Process(in Buffer) (Buffer, error) {
	if err := in.Validate(); err != nil {
		return Buffer{}, err
	}
	if len(in.Planes) != len(n.mapping) {
		return Buffer{}, fmt.Errorf("audio: channel count changed %d -> %d", len(n.mapping), len(in.Planes))
	}
	stereo := Buffer{Rate: in.Rate, Planes: [][]float32{make([]float32, in.Frames()), make([]float32, in.Frames())}, Layout: "stereo"}
	for c := range in.Planes {
		lg, rg := n.mapping[c][0], n.mapping[c][1]
		for i, v := range in.Planes[c] {
			stereo.Planes[0][i] += float32(float64(v) * lg)
			stereo.Planes[1][i] += float32(float64(v) * rg)
		}
	}
	out, err := n.res.Process(stereo)
	if err != nil {
		return Buffer{}, err
	}
	out.Layout = "stereo"
	return out, nil
}

// Flush drains the resampler delay as silence-padded output.
func (n *Normalizer) Flush() Buffer {
	out := n.res.Flush()
	out.Layout = "stereo"
	return out
}

// Reset clears resampler state so a restarted stream begins cleanly.
func (n *Normalizer) Reset() { n.res = NewResampler(n.res.srcRate, n.res.dstRate) }

// DownmixMatrix maps an input channel layout to two output gains per channel.
//
// Stereo passes through. Mono is duplicated. Formats with three or more
// channels use the standard ITU-R BS.775 coefficients: centre and surrounds at
// -3 dB, LFE excluded. Every matrix is scaled so a full-scale coherent input
// cannot exceed unity on either output.
func DownmixMatrix(channels int, layout string) ([][2]float64, error) {
	switch channels {
	case 1:
		return [][2]float64{{1, 1}}, nil
	case 2:
		return [][2]float64{{1, 0}, {0, 1}}, nil
	}
	if channels > 8 {
		return nil, fmt.Errorf("audio: unsupported channel count %d", channels)
	}
	const s = 0.7071067811865476 // -3 dB
	// Standard order: FL, FR, FC, LFE, BL, BR, SL, SR.
	m := make([][2]float64, channels)
	m[0] = [2]float64{1, 0}
	m[1] = [2]float64{0, 1}
	if channels >= 3 {
		m[2] = [2]float64{s, s}
	}
	if channels >= 4 {
		m[3] = [2]float64{0, 0}
	}
	if channels >= 6 {
		m[4] = [2]float64{s, 0}
		m[5] = [2]float64{0, s}
	}
	if channels >= 8 {
		m[6] = [2]float64{s, 0}
		m[7] = [2]float64{0, s}
	}
	// Normalize by the worst-case coherent sum per output.
	var maxL, maxR float64
	for _, g := range m {
		maxL += math.Abs(g[0])
		maxR += math.Abs(g[1])
	}
	norm := math.Max(maxL, maxR)
	if norm > 1 {
		for i := range m {
			m[i][0] /= norm
			m[i][1] /= norm
		}
	}
	return m, nil
}

// Resampler is a streaming windowed-sinc sample-rate converter. The kernel is
// a Kaiser-windowed sinc with a cutoff below Nyquist for the lower rate, so
// both up- and down-sampling avoid aliasing and imaging.
type Resampler struct {
	srcRate, dstRate int
	step             float64
	taps             int
	kernel           []float64
	buf              [][]float32
	phase            float64
	wrote            bool
}

// NewResampler creates a converter. A nil or identical-rate converter is a
// pass-through.
func NewResampler(srcRate, dstRate int) *Resampler {
	if srcRate <= 0 || dstRate <= 0 {
		return nil
	}
	const taps = 32
	r := &Resampler{srcRate: srcRate, dstRate: dstRate, taps: taps, step: float64(srcRate) / float64(dstRate)}
	ratio := float64(dstRate) / float64(srcRate)
	cutoff := 0.95
	if ratio < 1 {
		cutoff *= ratio
	}
	r.kernel = make([]float64, taps)
	half := float64(taps) / 2
	beta := 8.0
	sum := 0.0
	for i := 0; i < taps; i++ {
		x := float64(i) - half + 1
		arg := math.Pi * x * cutoff
		s := 1.0
		if arg != 0 {
			s = math.Sin(arg) / arg
		}
		w := kaiser((x+half)/float64(taps), beta)
		r.kernel[i] = s * cutoff * w
		sum += r.kernel[i]
	}
	if sum != 0 {
		for i := range r.kernel {
			r.kernel[i] /= sum
		}
	}
	return r
}

// Process resamples one interleaved set of channels.
func (r *Resampler) Process(in Buffer) (Buffer, error) {
	if r == nil || r.srcRate == r.dstRate {
		return in, nil
	}
	if err := in.Validate(); err != nil {
		return Buffer{}, err
	}
	if in.Rate != r.srcRate {
		return Buffer{}, fmt.Errorf("audio: resampler expects %d Hz, got %d", r.srcRate, in.Rate)
	}
	if r.buf == nil {
		r.buf = make([][]float32, len(in.Planes))
		for c := range r.buf {
			r.buf[c] = make([]float32, r.taps)
		}
	}
	if len(in.Planes) != len(r.buf) {
		return Buffer{}, fmt.Errorf("audio: resampler channel count changed")
	}
	for c := range in.Planes {
		r.buf[c] = append(r.buf[c], in.Planes[c]...)
	}
	r.wrote = true
	avail := len(r.buf[0]) - r.taps // usable samples after the history prefix
	var outN int
	if avail > 0 {
		outN = int(math.Floor((float64(avail-r.taps))/r.step)) + 1
		if outN < 0 {
			outN = 0
		}
	}
	out := Buffer{Rate: r.dstRate, Planes: make([][]float32, len(r.buf))}
	for c := range out.Planes {
		out.Planes[c] = make([]float32, outN)
	}
	pos := r.phase
	for n := 0; n < outN; n++ {
		base := int(math.Floor(pos))
		idx := base + r.taps/2 // kernel spans [pos-taps/2, pos+taps/2)
		for c := range r.buf {
			var acc float64
			for k := 0; k < r.taps; k++ {
				j := idx - r.taps/2 + k
				if j < 0 || j >= len(r.buf[c]) {
					continue
				}
				acc += float64(r.buf[c][j]) * r.kernel[k]
			}
			out.Planes[c][n] = float32(acc)
		}
		pos += r.step
	}
	// Drop consumed input, keeping kernel history.
	consumed := int(math.Floor(pos)) - r.taps/2
	if consumed > 0 {
		if consumed > len(r.buf[0])-r.taps {
			consumed = len(r.buf[0]) - r.taps
		}
		for c := range r.buf {
			r.buf[c] = append(r.buf[c][:0], r.buf[c][consumed:]...)
		}
		pos -= float64(consumed)
	}
	r.phase = pos
	return out, nil
}

// Flush returns the tail samples held back by the kernel and resets state.
func (r *Resampler) Flush() Buffer {
	if r == nil || r.srcRate == r.dstRate {
		return Buffer{Rate: 48000}
	}
	if !r.wrote {
		return Buffer{Rate: r.dstRate}
	}
	// Feed zeros to push the remaining history through the kernel.
	pad := make([][]float32, len(r.buf))
	for c := range pad {
		pad[c] = make([]float32, r.taps)
	}
	rest := r.buf
	r.buf = pad
	// Process the pad directly so the output covers the delayed tail.
	outN := int(math.Floor(float64(len(rest[0])-r.taps)/r.step)) + 1
	if outN < 0 {
		outN = 0
	}
	out := Buffer{Rate: r.dstRate, Planes: make([][]float32, len(rest))}
	pos := r.phase
	for c := range out.Planes {
		out.Planes[c] = make([]float32, outN)
	}
	for n := 0; n < outN; n++ {
		base := int(math.Floor(pos))
		idx := base + r.taps/2
		for c := range rest {
			var acc float64
			for k := 0; k < r.taps; k++ {
				j := idx - r.taps/2 + k
				if j < 0 || j >= len(rest[c]) {
					continue
				}
				acc += float64(rest[c][j]) * r.kernel[k]
			}
			out.Planes[c][n] = float32(acc)
		}
		pos += r.step
	}
	r.phase = 0
	r.wrote = false
	return out
}

func kaiser(x, beta float64) float64 {
	if x < 0 || x > 1 {
		return 0
	}
	d := 2*x - 1
	return besselI0(beta*math.Sqrt(1-d*d)) / besselI0(beta)
}

func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	for k := 1; k < 32; k++ {
		term *= (x / (2 * float64(k))) * (x / (2 * float64(k)))
		sum += term
		if term < 1e-12*sum {
			break
		}
	}
	return sum
}
