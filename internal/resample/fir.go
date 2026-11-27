// Package resample converts between sample rates with a 32-tap
// Blackman-windowed sinc FIR. The filter keeps its input history and its
// exact rational output phase across block boundaries, and Flush emits the
// deterministic tail using zero padding rather than carrying rounding error.
package resample

import (
	"fmt"
	"math"

	"avpipe/internal/media"
)

// Taps is the fixed FIR length required by the pipeline contract.
const Taps = 32

// half is the one-sided filter radius.
const half = Taps / 2

// Config describes a conversion job.
type Config struct {
	Src media.Rate
	Dst media.Rate
	// Cutoff scales the low-pass corner relative to the lower Nyquist
	// frequency. Values outside (0, 1] select the 0.92 default.
	Cutoff float64
	// MaxBlockFrames bounds an emitted output block. Zero selects 4096.
	MaxBlockFrames int
}

// Resampler streams interleaved normalized samples from Src to Dst.
type Resampler struct {
	src, dst media.Rate
	srcClock media.Clock
	track    string
	channels int
	maxBlock int
	coeffs   func(float64) float64

	buf       []float64 // interleaved input starting at absolute frame bufStart
	bufStart  int64
	bufFrames int64

	totalIn  int64
	nextOut  int64 // absolute output frame under construction
	totalOut int64 // exact output frame count, set at Flush

	nextEmit int64
	pending  []float64
	seq      uint64
	ended    bool
	flushed  bool
}

// New validates the conversion and builds the kernel.
func New(cfg Config) (*Resampler, error) {
	if !cfg.Src.Valid() || !cfg.Dst.Valid() {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "new",
			fmt.Errorf("invalid rates %s -> %s", cfg.Src.String(), cfg.Dst.String()))
	}
	cutoff := cfg.Cutoff
	if cutoff <= 0 || cutoff > 1 {
		cutoff = 0.92
	}
	maxBlock := cfg.MaxBlockFrames
	if maxBlock <= 0 {
		maxBlock = 4096
	}
	clock, err := media.NewClock(cfg.Src)
	if err != nil {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "new", err)
	}
	ratio := cfg.Dst.Float() / cfg.Src.Float()
	fc := 0.5 * cutoff * math.Min(1, ratio)
	return &Resampler{
		src:      cfg.Src,
		dst:      cfg.Dst,
		srcClock: clock,
		maxBlock: maxBlock,
		coeffs:   kernel(fc),
	}, nil
}

// kernel returns h(u) for one fractional tap distance u, in source frames.
// fc is the corner in cycles per source sample.
func kernel(fc float64) func(float64) float64 {
	return func(u float64) float64 {
		if u <= -half || u >= half {
			return 0
		}
		var sinc float64
		if u == 0 {
			sinc = 2 * fc
		} else {
			sinc = math.Sin(2*math.Pi*fc*u) / (math.Pi * u)
		}
		w := 0.42 + 0.5*math.Cos(math.Pi*u/float64(half)) + 0.08*math.Cos(2*math.Pi*u/float64(half))
		return sinc * w
	}
}

// exactPos returns the integer part and fraction of the absolute source
// position for output frame n: n * dstDen * srcNum / (dstNum * srcDen).
func (r *Resampler) exactPos(n int64) (base int64, frac float64, err error) {
	num, ok := mul(n, r.dst.Den)
	if !ok {
		return 0, 0, fmt.Errorf("output position overflow at frame %d", n)
	}
	if num, ok = mul(num, r.src.Num); !ok {
		return 0, 0, fmt.Errorf("output position overflow at frame %d", n)
	}
	den, ok := mul(r.dst.Num, r.src.Den)
	if !ok || den <= 0 {
		return 0, 0, fmt.Errorf("rate product overflow")
	}
	base = num / den
	rem := num % den
	if rem < 0 {
		rem += den
	}
	return base, float64(rem) / float64(den), nil
}

func mul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a == math.MinInt64 || b == math.MinInt64 {
		return 0, false
	}
	p := a * b
	if p/b != a {
		return 0, false
	}
	return p, true
}

// sample reads one channel of an absolute input frame, treating frames outside
// the buffered window as silence. Flush relies on that for zero padding.
func (r *Resampler) sample(frame int64, ch int) float64 {
	if frame < r.bufStart || frame >= r.bufStart+r.bufFrames {
		return 0
	}
	return r.buf[(frame-r.bufStart)*int64(r.channels)+int64(ch)]
}

// Process consumes one contiguous input block and returns complete output
// blocks. Every boundary comes from an exact absolute output index, so it
// cannot drift.
func (r *Resampler) Process(in media.Block) ([]media.Block, error) {
	if r.ended {
		return nil, media.NewError(media.PhaseResample, media.ExitInternal, in.Track, in.Seq, false, "process",
			fmt.Errorf("block after flush"))
	}
	if !in.Rate.Valid() || in.Channels <= 0 {
		return nil, media.NewError(media.PhaseResample, media.ExitInput, in.Track, in.Seq, false, "process",
			fmt.Errorf("invalid block metadata"))
	}
	if r.channels == 0 {
		r.channels = in.Channels
	} else if in.Channels != r.channels {
		return nil, media.NewError(media.PhaseResample, media.ExitInput, in.Track, in.Seq, false, "process",
			fmt.Errorf("channel count changed from %d to %d", r.channels, in.Channels))
	}
	if in.Rate != r.src {
		return nil, media.NewError(media.PhaseResample, media.ExitInput, in.Track, in.Seq, false, "process",
			fmt.Errorf("block rate %s != configured %s", in.Rate.String(), r.src.String()))
	}
	if in.Start != r.totalIn {
		return nil, media.NewError(media.PhaseResample, media.ExitInput, in.Track, in.Seq, false, "process",
			fmt.Errorf("non-contiguous input: start %d, expected %d", in.Start, r.totalIn))
	}
	if len(in.Samples)%in.Channels != 0 {
		return nil, media.NewError(media.PhaseResample, media.ExitInput, in.Track, in.Seq, false, "process",
			fmt.Errorf("sample count %d not divisible by %d channels", len(in.Samples), in.Channels))
	}
	r.track = in.Track
	frames := in.Frames()
	r.buf = append(r.buf, in.Samples...)
	r.bufFrames += int64(frames)
	r.totalIn += int64(frames)
	// Never emit past the exact total implied by the input seen so far; the
	// remainder is produced by Flush once the true end of stream is known.
	cap, err := r.outputTotal(r.totalIn)
	if err != nil {
		return nil, media.NewError(media.PhaseResample, media.ExitInternal, in.Track, in.Seq, false, "emit", err)
	}
	return r.emitAvailable(false, cap, true)
}

// outputTotal is the exact number of output frames for framesIn input frames,
// using the same rational conversion as Clock.Convert (round halves away from
// zero).
func (r *Resampler) outputTotal(framesIn int64) (int64, error) {
	return r.srcClock.Convert(framesIn, r.dst)
}

// emitAvailable produces every output frame whose rightmost tap is buffered
// and, when hasCap is set, whose index is below cap. With final set it runs
// to the exact total, reading silence past the end. The cap keeps a streaming
// emission from ever passing the exact rational total, which only becomes
// known for certain at Flush: for heavy downsampling the kernel's reach in
// output frames is short, so the "buffered" test alone could overshoot.
func (r *Resampler) emitAvailable(final bool, cap int64, hasCap bool) ([]media.Block, error) {
	var out []media.Block
	for {
		if final {
			if r.nextOut >= r.totalOut {
				break
			}
		} else {
			if hasCap && r.nextOut >= cap {
				break
			}
			base, _, err := r.exactPos(r.nextOut)
			if err != nil {
				return nil, err
			}
			if base+half >= r.bufStart+r.bufFrames {
				break
			}
		}
		base, frac, err := r.exactPos(r.nextOut)
		if err != nil {
			return nil, err
		}
		for c := 0; c < r.channels; c++ {
			var acc, norm float64
			for k := base - half + 1; k <= base+half; k++ {
				h := r.coeffs(float64(k-base) - frac)
				norm += h
				acc += r.sample(k, c) * h
			}
			if norm != 0 {
				acc /= norm
			}
			r.pending = append(r.pending, acc)
		}
		r.nextOut++
		out = append(out, r.drain()...)
	}
	out = append(out, r.drain()...)
	r.dropConsumed()
	return out, nil
}

// drain emits complete pending blocks.
func (r *Resampler) drain() []media.Block {
	perBlock := r.maxBlock * r.channels
	var out []media.Block
	for len(r.pending) >= perBlock {
		out = append(out, r.wrap(r.pending[:perBlock]))
		r.pending = append([]float64(nil), r.pending[perBlock:]...)
	}
	return out
}

func (r *Resampler) wrap(samples []float64) media.Block {
	frames := int64(len(samples) / r.channels)
	b := media.Block{
		Track:    r.track,
		Seq:      r.seq,
		Start:    r.nextEmit,
		Rate:     r.dst,
		Channels: r.channels,
		Samples:  samples,
	}
	r.seq++
	r.nextEmit += frames
	return b
}

// dropConsumed discards input frames no longer reachable by the kernel.
func (r *Resampler) dropConsumed() {
	base, _, err := r.exactPos(r.nextOut)
	if err != nil {
		return
	}
	keepFrom := base - half + 1
	if keepFrom > r.bufStart+r.bufFrames {
		keepFrom = r.bufStart + r.bufFrames
	}
	if keepFrom <= r.bufStart {
		return
	}
	drop := (keepFrom - r.bufStart) * int64(r.channels)
	r.buf = append([]float64(nil), r.buf[drop:]...)
	r.bufStart = keepFrom
	r.bufFrames = int64(len(r.buf)) / int64(r.channels)
}

// Flush emits the deterministic tail padded with silence. It is idempotent:
// repeated calls return no blocks.
func (r *Resampler) Flush() ([]media.Block, error) {
	if r.ended || r.flushed {
		return nil, nil
	}
	r.ended = true
	if r.channels == 0 {
		r.flushed = true
		return nil, nil
	}
	var err error
	r.totalOut, err = r.outputTotal(r.totalIn)
	if err != nil {
		return nil, media.NewError(media.PhaseResample, media.ExitInternal, r.track, 0, false, "flush", err)
	}
	pad := int64(half + 1)
	r.buf = append(r.buf, make([]float64, pad*int64(r.channels))...)
	r.bufFrames += pad
	out, err := r.emitAvailable(true, 0, false)
	if err != nil {
		return nil, media.NewError(media.PhaseResample, media.ExitInternal, r.track, 0, false, "flush", err)
	}
	if len(r.pending) > 0 {
		out = append(out, r.wrap(r.pending))
		r.pending = nil
	}
	r.flushed = true
	return out, nil
}

// TotalOut is valid after Flush and reports the exact output frame count.
func (r *Resampler) TotalOut() int64 { return r.totalOut }

// Channels reports the negotiated channel count (zero before the first block).
func (r *Resampler) Channels() int { return r.channels }
