package audio

import (
	"math"

	"avpipe/internal/clock"
	"avpipe/internal/fault"
)

// Resampler converts a stream of blocks to a target sample rate with a
// windowed-sinc interpolator. Filter history is carried across blocks, so
// results never depend on how the input was chunked. Output positions live
// on an exact rational grid (outFrame*srcRate/dstRate), so phase error cannot
// accumulate over long streams.
//
// History is a gap buffer trimmed after each Slurp. Its size is bounded by
// the largest pushed block plus the filter width, never by stream length.
type Resampler struct {
	srcRate  int
	dstRate  int
	channels int
	taps     int
	half     int
	cutoff   float64
	layout   Layout

	hist    [][]float64 // hist[c][i] is absolute input frame base+i
	base    int64
	written int64
	out     int64

	phaseCache map[int64][]float64
}

// ResamplerConfig describes one conversion.
type ResamplerConfig struct {
	SrcRate int
	DstRate int
	Layout  Layout
	Taps    int
}

// NewResampler validates the conversion and prepares filter state.
func NewResampler(cfg ResamplerConfig) (*Resampler, error) {
	if cfg.SrcRate < minSampleRate || cfg.SrcRate > maxSampleRate ||
		cfg.DstRate < minSampleRate || cfg.DstRate > maxSampleRate {
		return nil, fault.New(fault.CodeResample, "audio.resample",
			"unsupported rates %d -> %d", cfg.SrcRate, cfg.DstRate)
	}
	if err := cfg.Layout.Validate(); err != nil {
		return nil, err
	}
	taps := cfg.Taps
	if taps == 0 {
		taps = 17
	}
	if taps < 3 || taps%2 == 0 || taps > 257 {
		return nil, fault.New(fault.CodeResample, "audio.resample",
			"tap count %d must be odd and within 3..257", taps)
	}
	cutoff := 1.0
	if cfg.DstRate < cfg.SrcRate {
		cutoff = 0.95 * float64(cfg.DstRate) / float64(cfg.SrcRate)
	}
	r := &Resampler{
		srcRate: cfg.SrcRate, dstRate: cfg.DstRate,
		channels: len(cfg.Layout.Channels), layout: cfg.Layout,
		taps: taps, half: taps / 2, cutoff: cutoff,
		hist:       make([][]float64, len(cfg.Layout.Channels)),
		phaseCache: map[int64][]float64{},
	}
	return r, nil
}

// weights returns the unity-gain interpolation kernel for a fractional
// output phase. Phases repeat on the rational grid, so results are memoized;
// the cache is capped so pathological ratios cannot grow memory without
// bound (recomputation stays deterministic).
func (r *Resampler) weights(rem int64) []float64 {
	if w, ok := r.phaseCache[rem]; ok {
		return w
	}
	frac := float64(rem) / float64(r.dstRate)
	mid := float64(r.half)
	w := make([]float64, r.taps)
	var sum float64
	for k := 0; k < r.taps; k++ {
		t := float64(k) - mid - frac
		var sinc float64
		if t == 0 {
			sinc = 1
		} else {
			x := math.Pi * r.cutoff * t
			sinc = math.Sin(x) / x
		}
		win := 0.5 * (1 + math.Cos(math.Pi*t/(mid+1)))
		w[k] = sinc * win
		sum += w[k]
	}
	for k := range w {
		w[k] /= sum
	}
	if len(r.phaseCache) < 512 {
		r.phaseCache[rem] = w
	}
	return w
}

func (r *Resampler) sampleAt(ch int, idx int64) float64 {
	off := idx - r.base
	if idx < 0 || idx >= r.written || off < 0 || off >= int64(len(r.hist[ch])) {
		return 0
	}
	return r.hist[ch][off]
}

// trim drops history strictly older than keepFrom. The filter never reads
// more than half bytes behind the next output position, so this bounds the
// buffer by the block size plus the filter width.
func (r *Resampler) trim(keepFrom int64) {
	if keepFrom <= r.base {
		return
	}
	drop := keepFrom - r.base
	if drop > int64(len(r.hist[0])) {
		drop = int64(len(r.hist[0]))
	}
	for c := range r.hist {
		r.hist[c] = append(r.hist[c][:0], r.hist[c][drop:]...)
	}
	r.base += drop
}

// Push copies block frames into filter history.
func (r *Resampler) Push(b Block) error {
	if b.Frames < 0 || len(b.Data) != b.Frames*r.channels {
		return fault.New(fault.CodeResample, "audio.resample",
			"block has %d values, want %d frames x %d channels",
			len(b.Data), b.Frames, r.channels)
	}
	if b.SampleRate != r.srcRate {
		return fault.New(fault.CodeResample, "audio.resample",
			"block rate %d does not match resampler input %d", b.SampleRate, r.srcRate)
	}
	if b.Layout.String() != r.layout.String() {
		return fault.New(fault.CodeResample, "audio.resample",
			"block layout %s does not match resampler input %s", b.Layout, r.layout)
	}
	for c := range r.hist {
		for f := 0; f < b.Frames; f++ {
			r.hist[c] = append(r.hist[c], b.Data[f*r.channels+c])
		}
	}
	r.written += int64(b.Frames)
	return nil
}

// Slurp drains every output frame the written input can support.
// endOfStream releases the filter tail by zero-padding after the last frame.
// The emitted total is capped at ceil(written*dst/src) so the final tail is
// complete without inventing extra duration beyond one frame.
func (r *Resampler) Slurp(endOfStream bool, blockFrames int) []Block {
	if r.written == 0 {
		return nil
	}
	limit := r.written - 1 - int64(r.half)
	maxOut := int64(1) << 62
	if endOfStream {
		limit = r.written + int64(r.half)
		maxOut = (r.written*int64(r.dstRate) + int64(r.srcRate) - 1) / int64(r.srcRate)
	}
	if limit < 0 || r.out >= maxOut {
		return nil
	}
	var data []float64
	start := r.out
	for start < maxOut {
		num := start * int64(r.srcRate)
		pos := num / int64(r.dstRate)
		if pos > limit {
			break
		}
		w := r.weights(num % int64(r.dstRate))
		base := pos - int64(r.half)
		for c := 0; c < r.channels; c++ {
			var acc float64
			for k := 0; k < r.taps; k++ {
				acc += r.sampleAt(c, base+int64(k)) * w[k]
			}
			data = append(data, acc)
		}
		start++
	}
	frames := int(start - r.out)
	if frames == 0 {
		r.trim(r.out*int64(r.srcRate)/int64(r.dstRate) - int64(r.half))
		return nil
	}
	var blocks []Block
	for off := 0; off < frames; off += blockFrames {
		n := blockFrames
		if off+n > frames {
			n = frames - off
		}
		tick, _ := clock.SamplesToTicks(r.out+int64(off), r.dstRate)
		blocks = append(blocks, Block{
			StartFrame: r.out + int64(off), StartTick: tick,
			SampleRate: r.dstRate, Layout: r.layout, Frames: n,
			Data: append([]float64(nil), data[off*r.channels:(off+n)*r.channels]...),
		})
	}
	r.out = start
	r.trim(r.out*int64(r.srcRate)/int64(r.dstRate) - int64(r.half))
	return blocks
}

// OutFrames is the number of emitted output frames.
func (r *Resampler) OutFrames() int64 { return r.out }

// RoundingError reports the cumulative rounding error in source samples: the
// gap between the ideal continuous output duration and the integer number of
// emitted frames. It grows from integer frame quantization only.
func (r *Resampler) RoundingError() float64 {
	if r.written == 0 {
		return 0
	}
	ideal := float64(r.written) * float64(r.dstRate) / float64(r.srcRate)
	return math.Abs(ideal-float64(r.out)) * float64(r.srcRate) / float64(r.dstRate)
}
