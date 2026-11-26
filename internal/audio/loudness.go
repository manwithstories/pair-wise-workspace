package audio

import (
	"errors"
	"fmt"
	"math"

	"github.com/asticode/go-astiav"

	"mediamix/internal/clock"
)

// Loudness targets from the delivery specification.
const (
	TargetLUFS  = -18.0
	MaxTruePeak = -1.0 // dBTP
)

// LoudnessNormaliser runs every frame through an FFmpeg filter graph:
// loudnorm (linear, deterministic) followed by alimiter as the true-peak
// safety ceiling. The graph has no adaptive second pass, so equal inputs
// always produce byte-identical outputs — required for replay hashing.
type LoudnessNormaliser struct {
	graph   *astiav.FilterGraph
	src     *astiav.BuffersrcFilterContext
	sink    *astiav.BuffersinkFilterContext
	rate    int
	channels int
	closed  bool
}

// NewLoudnessNormaliser builds the filter graph for 48 kHz stereo float.
func NewLoudnessNormaliser(rate, channels int) (*LoudnessNormaliser, error) {
	if rate <= 0 || channels <= 0 {
		return nil, clock.NewError(clock.StageLoudness, clock.ErrInvalidParam, 0, false,
			"rate and channels must be positive")
	}
	g := astiav.AllocFilterGraph()
	if g == nil {
		return nil, clock.NewError(clock.StageLoudness, clock.ErrInvalidParam, 0, false,
			"alloc filter graph failed")
	}
	abuf := astiav.FindFilterByName("abuffer")
	asink := astiav.FindFilterByName("abuffersink")
	if abuf == nil || asink == nil {
		g.Free()
		return nil, clock.NewError(clock.StageLoudness, clock.ErrInvalidParam, 0, false,
			"abuffer/abuffersink filters unavailable")
	}
	src, err := g.NewBuffersrcFilterContext(abuf, "in")
	if err != nil {
		g.Free()
		return nil, clock.WrapError(clock.StageLoudness, clock.ErrInvalidParam, 0, false, "abuffer", err)
	}
	sink, err := g.NewBuffersinkFilterContext(asink, "out")
	if err != nil {
		g.Free()
		return nil, clock.WrapError(clock.StageLoudness, clock.ErrInvalidParam, 0, false, "abuffersink", err)
	}

	params := astiav.AllocBuffersrcFilterContextParameters()
	defer params.Free()
	params.SetSampleFormat(astiav.SampleFormatFlt)
	params.SetSampleRate(rate)
	params.SetChannelLayout(astiav.ChannelLayoutStereo)
	params.SetTimeBase(astiav.NewRational(1, rate))
	if err := src.SetParameters(params); err != nil {
		g.Free()
		return nil, clock.WrapError(clock.StageLoudness, clock.ErrInvalidParam, 0, false, "src params", err)
	}
	if err := src.Initialize(nil); err != nil {
		g.Free()
		return nil, clock.WrapError(clock.StageLoudness, clock.ErrInvalidParam, 0, false, "src init", err)
	}

	outputs := astiav.AllocFilterInOut()
	defer outputs.Free()
	inputs := astiav.AllocFilterInOut()
	defer inputs.Free()
	outputs.SetName("in")
	outputs.SetFilterContext(src.FilterContext())
	outputs.SetPadIdx(0)
	inputs.SetName("out")
	inputs.SetFilterContext(sink.FilterContext())
	inputs.SetPadIdx(0)

	// Linear loudnorm avoids dynamic gain riding; alimiter guarantees the
	// true-peak ceiling. loudnorm internally measures at 192 kHz in double,
	// so aformat pins the chain output back to the delivery format.
	chain := fmt.Sprintf(
		"loudnorm=I=%.1f:TP=%.1f:LRA=11:linear=true,alimiter=limit=%.6f:level=false,aformat=sample_fmts=flt:channel_layouts=stereo:sample_rates=%d",
		TargetLUFS, MaxTruePeak, math.Pow(10, MaxTruePeak/20), rate)
	if err := g.Parse(chain, inputs, outputs); err != nil {
		g.Free()
		return nil, clock.WrapError(clock.StageLoudness, clock.ErrInvalidParam, 0, false,
			"parse loudnorm chain", err)
	}
	if err := g.Configure(); err != nil {
		g.Free()
		return nil, clock.WrapError(clock.StageLoudness, clock.ErrInvalidParam, 0, false,
			"configure filter graph", err)
	}
	return &LoudnessNormaliser{graph: g, src: src, sink: sink, rate: rate, channels: channels}, nil
}

// frameFromF32 wraps interleaved stereo float samples in an AVFrame.
func (l *LoudnessNormaliser) frameFromF32(samples []float32, pts int64) *astiav.Frame {
	f := astiav.AllocFrame()
	f.SetSampleFormat(astiav.SampleFormatFlt)
	f.SetSampleRate(l.rate)
	f.SetChannelLayout(astiav.ChannelLayoutStereo)
	f.SetNbSamples(len(samples) / l.channels)
	f.SetPts(pts)
	return f
}

// Process pushes one stereo frame through the graph and returns the
// normalised frames it yields (the graph may buffer internally).
func (l *LoudnessNormaliser) Process(samples []float32, pts int64) ([]float32, error) {
	if l.closed {
		return nil, clock.NewError(clock.StageLoudness, clock.ErrInvalidParam, 0, false,
			"normaliser used after close")
	}
	in := l.frameFromF32(samples, pts)
	if err := in.AllocBuffer(0); err != nil {
		in.Free()
		return nil, clock.WrapError(clock.StageLoudness, clock.ErrInvalidPacket, 0, false, "alloc in", err)
	}
	writeF32(in, samples)
	if err := l.src.AddFrame(in, astiav.NewBuffersrcFlags(0)); err != nil {
		in.Free()
		return nil, clock.WrapError(clock.StageLoudness, clock.ErrInvalidPacket, 0, true, "add frame", err)
	}
	in.Free()
	return l.drain(false)
}

// Flush signals end of stream and returns the buffered tail frames.
func (l *LoudnessNormaliser) Flush() ([]float32, error) {
	if l.closed {
		return nil, nil
	}
	if err := l.src.AddFrame(nil, astiav.NewBuffersrcFlags(0)); err != nil {
		return nil, clock.WrapError(clock.StageLoudness, clock.ErrInvalidPacket, 0, false, "eof", err)
	}
	out, err := l.drain(true)
	l.Close()
	return out, err
}

func (l *LoudnessNormaliser) drain(eof bool) ([]float32, error) {
	var acc []float32
	for {
		f := astiav.AllocFrame()
		err := l.sink.GetFrame(f, astiav.NewBuffersinkFlags())
		if err != nil {
			f.Free()
			// EAGAIN: graph needs more input; EOF: flush finished. Both are
			// normal drain terminators, not processing failures.
			if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
				return acc, nil
			}
			return acc, clock.WrapError(clock.StageLoudness, clock.ErrInvalidPacket, 0, true,
				"filter sink get frame", err)
		}
		// The sink may return a frame whose allocated plane (linesize) is
		// larger than the meaningful NbSamples; never read past the plane.
		n := f.NbSamples() * l.channels
		if maxN := f.Linesize()[0] / 4; n > maxN {
			n = maxN
		}
		got := readF32(f, n)
		for i, v := range got {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return acc, clock.NewError(clock.StageLoudness, clock.ErrInvalidPacket, 0, false,
					fmt.Sprintf("filter produced non-finite sample at %d", i))
			}
		}
		acc = append(acc, got...)
		f.Free()
	}
}

// Close releases the filter graph.
func (l *LoudnessNormaliser) Close() {
	if l.closed {
		return
	}
	l.closed = true
	l.graph.Free()
}
