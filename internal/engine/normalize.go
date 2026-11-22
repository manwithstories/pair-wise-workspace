// Streaming media kernel primitives: rational resampling, channel-layout
// normalisation and peak/loudness control.
//
// All position arithmetic is integer-rational. The resampler's output frame
// count for N input frames is exactly round((N-1)*dst/src)+1, so the total
// time error of a stream is zero, not merely small; hostile or unusual rate
// pairs cannot drift.
package engine

import (
	"encoding/binary"
	"math"

	"mediacore/internal/media"
)

// Config is a fully-defaulted processing configuration.
type Config struct {
	OutputRate     int
	OutputChannels int
	// LoudnessDBFS is the peak ceiling in dBFS, e.g. -1.
	LoudnessDBFS float64
	// BlockFrames is the maximum frames per output block; never more than
	// one second of output at OutputRate.
	BlockFrames int
}

// DefaultBlockFrames is the block size used when none is configured.
const DefaultBlockFrames = 4800 // 100 ms at 48 kHz

// Resampler converts a mono/stereo stream between integer rates with a
// 16-tap Hann-windowed sinc kernel and exact rational phase.
//
// Memory is bounded by the pending window: roughly (taps + frames pushed in
// one call) input frames, independent of stream length.
type Resampler struct {
	src, dst int
	channels int

	// stepNum/stepDen is the reduced input-frames-per-output-frame step.
	stepNum, stepDen int64
	phase            int64 // numerator of the fractional cursor position

	// pending holds interleaved input frames not yet consumed; base is the
	// absolute input frame index of pending[0].
	pending []float64
	base    int64

	nextPos int64 // absolute integer input frame of the next output
	totalIn int64
	emitted int64
	taps    int
	flushed bool
}

// NewResampler prepares a resampler. channels must be 1 or 2 and both rates
// must be positive. A src == dst resampler is an exact pass-through.
func NewResampler(src, dst, channels int) (*Resampler, error) {
	if src <= 0 || dst <= 0 {
		return nil, media.NewError(media.CodeInvalidSampleRate, -1, 0, "resampler rate %d -> %d", src, dst)
	}
	if channels != 1 && channels != 2 {
		return nil, media.NewError(media.CodeChannelsInvalid, -1, 0, "resampler channels %d", channels)
	}
	g := gcd64(int64(src), int64(dst))
	return &Resampler{
		src:      src,
		dst:      dst,
		channels: channels,
		stepNum:  int64(src) / g,
		stepDen:  int64(dst) / g,
		taps:     16,
	}, nil
}

// Reset clears stream state so the resampler can be reused.
func (r *Resampler) Reset() {
	r.pending = r.pending[:0]
	r.base = 0
	r.nextPos = 0
	r.phase = 0
	r.totalIn = 0
	r.emitted = 0
	r.flushed = false
}

// OutputFramesFor returns the exact number of output frames for n input
// frames, chosen so the rendered duration differs from the input duration
// by less than half an output sample: round(n*dst/src), integer arithmetic.
func (r *Resampler) OutputFramesFor(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if r.src == r.dst {
		return n
	}
	num := n * int64(r.dst)
	den := int64(r.src)
	q := num / den
	if rem := num % den; rem*2 >= den {
		q++
	}
	return q
}

// Push appends interleaved input frames and returns out extended with every
// output frame whose kernel window is fully inside the input seen so far.
func (r *Resampler) Push(in []float64, out []float64) []float64 {
	if len(in) == 0 {
		return out
	}
	if r.flushed {
		r.Reset()
	}
	frames := len(in) / r.channels
	r.pending = append(r.pending, in[:frames*r.channels]...)
	r.totalIn += int64(frames)

	if r.src == r.dst {
		out = append(out, r.pending...)
		r.emitted += int64(frames)
		r.pending = r.pending[:0]
		r.base = r.totalIn
		return out
	}

	half := int64(r.taps / 2)
	have := r.base + int64(len(r.pending)/r.channels)
	for r.nextPos+half < have {
		out = r.emit(out)
	}
	r.trim(half)
	return out
}

// Flush emits the remaining output frames with zero padding past the end of
// input. The total emitted frame count is exactly OutputFramesFor(totalIn).
func (r *Resampler) Flush(out []float64) []float64 {
	if r.flushed {
		return out
	}
	r.flushed = true
	if r.src == r.dst {
		return out
	}
	want := r.OutputFramesFor(r.totalIn)
	for r.emitted < want {
		out = r.emit(out)
	}
	r.pending = r.pending[:0]
	r.base = r.totalIn
	return out
}

// emit appends one output frame at the cursor and advances it.
func (r *Resampler) emit(out []float64) []float64 {
	frac := float64(r.phase) / float64(r.stepDen)
	for ch := 0; ch < r.channels; ch++ {
		out = append(out, r.interpolate(ch, frac))
	}
	r.emitted++
	r.phase += r.stepNum
	r.nextPos += r.phase / r.stepDen
	r.phase %= r.stepDen
	return out
}

// trim drops pending frames that no future kernel window can reach.
func (r *Resampler) trim(half int64) {
	low := r.nextPos - half + 1
	drop := low - r.base
	if drop <= 0 {
		return
	}
	if drop > int64(len(r.pending)/r.channels) {
		drop = int64(len(r.pending) / r.channels)
	}
	keep := r.pending[int(drop)*r.channels:]
	r.pending = append(r.pending[:0], keep...)
	r.base += drop
}

// sample returns the interleaved input value at absolute frame i; frames at
// or past the end of input read as zero (flush tail).
func (r *Resampler) sample(i int64, ch int) float64 {
	if i < r.base || i >= r.totalIn {
		return 0
	}
	idx := (i-r.base)*int64(r.channels) + int64(ch)
	if idx < 0 || idx >= int64(len(r.pending)) {
		return 0
	}
	return r.pending[idx]
}

// interpolate computes one output value at integer position nextPos plus
// frac, using a Hann-windowed sinc at the appropriate cutoff.
func (r *Resampler) interpolate(ch int, frac float64) float64 {
	cut := 1.0
	if r.dst < r.src {
		cut = float64(r.dst) / float64(r.src)
	}
	half := r.taps / 2
	first := r.nextPos - int64(half) + 1
	var acc, wsum float64
	for k := 0; k < r.taps; k++ {
		t := float64(k) - float64(half) - frac
		x := sinc(cut*t) * cut
		w := 0.5 + 0.5*math.Cos(math.Pi*t/float64(half))
		if w < 0 {
			w = 0
		}
		v := r.sample(first+int64(k), ch)
		acc += v * x * w
		wsum += x * w
	}
	if wsum != 0 {
		acc /= wsum
	}
	return acc
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

func gcd64(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

// Loudness and framing kernel: tone controls and 16-bit rendering.

// limitBlock applies the peak ceiling and classifies the block.
func limitBlock(frames []float64, ceilingDBFS float64) blockStat {
	ceiling := math.Pow(10, ceilingDBFS/20)
	var peak float64
	var clipped bool
	for _, v := range frames {
		a := math.Abs(v)
		if a >= 0.999 {
			clipped = true
		}
		if a > peak {
			peak = a
		}
	}
	stat := blockStat{peakLinear: peak, peakDBFS: linearToDBFS(peak)}
	stat.silent = peak == 0
	stat.clipped = clipped
	if peak > ceiling {
		stat.limited = true
		scale := ceiling / peak
		for i := range frames {
			frames[i] *= scale
		}
		stat.peakLinear = ceiling
		stat.peakDBFS = linearToDBFS(ceiling)
	}
	return stat
}

type blockStat struct {
	peakLinear float64
	peakDBFS   float64
	silent     bool
	clipped    bool
	limited    bool
}

// normalizeChannels maps the input layout onto the output layout.
func normalizeChannels(dst, src []float64, in, out int) {
	if in == out {
		copy(dst, src)
		return
	}
	n := len(src) / in
	if in == 1 && out == 2 {
		for i := n - 1; i >= 0; i-- {
			dst[2*i] = src[i]
			dst[2*i+1] = src[i]
		}
		return
	}
	if in == 2 && out == 1 {
		// Average keeps a centred source at constant loudness and cannot
		// introduce clipping.
		for i := 0; i < n; i++ {
			dst[i] = 0.5 * (src[2*i] + src[2*i+1])
		}
	}
}

// quantize renders interleaved float frames as signed 16-bit little-endian.
func quantize(frames []float64) []byte {
	out := make([]byte, len(frames)*2)
	for i, v := range frames {
		x := int(math.Round(v * 32767))
		if x > 32767 {
			x = 32767
		} else if x < -32768 {
			x = -32768
		}
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(x)))
	}
	return out
}

func linearToDBFS(v float64) float64 {
	if v <= 0 {
		return -120
	}
	db := 20 * math.Log10(v)
	if db < -120 {
		return -120
	}
	return db
}
