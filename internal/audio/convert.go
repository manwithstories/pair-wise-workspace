// Package audio converts deterministic PCM fixtures to 48 kHz stereo with
// explicit channel mapping, applies gap-fill policy, and measures/limits
// loudness. Every failure carries the track id and an exact time interval.
package audio

import (
	"math"

	"relaycheck/internal/media"
)

// TargetRate is the mandated output sample rate.
const TargetRate = 48000

// FramesPerSegment is the canonical 20 ms segment at 48 kHz.
const FramesPerSegment = 960

// PCM is interleaved signed-16-bit audio.
type PCM struct {
	Rate     int
	Channels int
	Samples  []int16
}

// Frames returns the number of complete interleaved frames.
func (p *PCM) Frames() int {
	if p == nil || p.Channels <= 0 {
		return 0
	}
	return len(p.Samples) / p.Channels
}

// FrameGap is a half-open output-frame interval [StartFrame, EndFrame).
type FrameGap struct {
	StartFrame int
	EndFrame   int
	Range      media.TimeRange
}

// GapPolicy decides how detected transport gaps are handled.
type GapPolicy int

const (
	// GapSilence zero-fills missing intervals and keeps the segment valid.
	GapSilence GapPolicy = iota
	// GapMarkUnrecoverable rejects the interval for downstream marking.
	GapMarkUnrecoverable
)

// Options configures conversion.
type Options struct {
	TrackID        int64
	TargetRate     int
	OutputChannels int
	ExpectedFrames int
	GapPolicy      GapPolicy
	Gaps           []FrameGap
	SegmentRange   media.TimeRange
}

// ChannelMap is an explicit fixed-point mixing matrix.
// out[o] = clamp(sum(Matrix[o][i]*in[i]) / Den).
type ChannelMap struct {
	In, Out int
	Den     int64
	Matrix  [][]int64
}

// StereoMap returns the documented mapping to stereo for 1/2/6 sources.
func StereoMap(inChannels int) (ChannelMap, *media.Fault) {
	const d = 1024
	switch inChannels {
	case 1:
		return ChannelMap{In: 1, Out: 2, Den: d, Matrix: [][]int64{{d}, {d}}}, nil
	case 2:
		return ChannelMap{In: 2, Out: 2, Den: d, Matrix: [][]int64{{d, 0}, {0, d}}}, nil
	case 6: // FL FR C LFE SL SR -> stereo downmix
		return ChannelMap{In: 6, Out: 2, Den: d, Matrix: [][]int64{
			{d, 0, 724, 0, 724, 0},
			{0, d, 724, 0, 0, 724},
		}}, nil
	default:
		return ChannelMap{}, media.NewFault(media.CodeEmptyChannel, "no explicit stereo mapping for %d channels", inChannels)
	}
}

func clamp16(v int64) int16 {
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return int16(v)
}

// MapChannels applies an explicit matrix.
func MapChannels(src *PCM, m ChannelMap, trackID int64) (*PCM, *media.Fault) {
	if src == nil || src.Channels <= 0 {
		return nil, media.NewFault(media.CodeEmptyChannel, "source has no channels").WithTrack(trackID)
	}
	if src.Channels != m.In {
		return nil, media.NewFault(media.CodeEmptyChannel, "channel map expects %d, source has %d", m.In, src.Channels).WithTrack(trackID)
	}
	frames := src.Frames()
	out := &PCM{Rate: src.Rate, Channels: m.Out, Samples: make([]int16, frames*m.Out)}
	for f := 0; f < frames; f++ {
		for o := 0; o < m.Out; o++ {
			var sum int64
			for i := 0; i < m.In; i++ {
				sum += m.Matrix[o][i] * int64(src.Samples[f*m.In+i])
			}
			out.Samples[f*m.Out+o] = clamp16(sum / m.Den)
		}
	}
	return out, nil
}

// Resample converts a stereo PCM to targetRate using exact fixed-point linear
// interpolation (no floating-point drift).
func Resample(src *PCM, targetRate int, trackID int64) (*PCM, *media.Fault) {
	if targetRate <= 0 {
		return nil, media.NewFault(media.CodeInvalidInput, "target rate must be positive").WithTrack(trackID)
	}
	inFrames := src.Frames()
	if inFrames == 0 {
		return nil, media.NewFault(media.CodeEmptyChannel, "cannot resample empty PCM").WithTrack(trackID)
	}
	if src.Rate == targetRate {
		cp := make([]int16, len(src.Samples))
		copy(cp, src.Samples)
		return &PCM{Rate: targetRate, Channels: src.Channels, Samples: cp}, nil
	}
	outFrames := int(int64(inFrames) * int64(targetRate) / int64(src.Rate))
	out := &PCM{Rate: targetRate, Channels: src.Channels, Samples: make([]int16, outFrames*src.Channels)}
	ch := src.Channels
	for o := 0; o < outFrames; o++ {
		// src position = o * inFrames / outFrames (same as o*rate/target).
		num := int64(o) * int64(inFrames)
		idx := num / int64(outFrames)
		fracNum := num % int64(outFrames)
		for c := 0; c < ch; c++ {
			a := int64(src.Samples[idx*int64(ch)+int64(c)])
			b := a
			if idx+1 < int64(inFrames) {
				b = int64(src.Samples[(idx+1)*int64(ch)+int64(c)])
			}
			v := a + (b-a)*fracNum/int64(outFrames)
			out.Samples[o*ch+c] = clamp16(v)
		}
	}
	return out, nil
}

// Convert runs validation, explicit channel mapping and exact resampling, then
// applies transport gaps and verifies the expected frame count.
func Convert(src *PCM, opts Options) (*PCM, *media.Fault) {
	trackID := opts.TrackID
	target := opts.TargetRate
	if target == 0 {
		target = TargetRate
	}
	outCh := opts.OutputChannels
	if outCh == 0 {
		outCh = 2
	}
	if src == nil || src.Channels <= 0 || len(src.Samples) == 0 {
		f := media.NewFault(media.CodeEmptyChannel, "track has empty audio channel").WithTrack(trackID)
		return nil, f.WithRange(opts.SegmentRange.Start, opts.SegmentRange.End)
	}

	if outCh == 2 {
		mp, f := StereoMap(src.Channels)
		if f != nil {
			return nil, f.WithTrack(trackID).WithRange(opts.SegmentRange.Start, opts.SegmentRange.End)
		}
		mapped, f := MapChannels(src, mp, trackID)
		if f != nil {
			return nil, f.WithRange(opts.SegmentRange.Start, opts.SegmentRange.End)
		}
		src = mapped
	}

	out, f := Resample(src, target, trackID)
	if f != nil {
		return nil, f.WithRange(opts.SegmentRange.Start, opts.SegmentRange.End)
	}

	for _, g := range opts.Gaps {
		if opts.GapPolicy == GapMarkUnrecoverable {
			f := media.NewFault(media.CodeUnrecoverableGap, "unrecoverable transport gap frames [%d,%d)", g.StartFrame, g.EndFrame).
				WithTrack(trackID)
			if g.Range.IsSet() {
				f.WithRange(g.Range.Start, g.Range.End)
			} else {
				f.WithRange(opts.SegmentRange.Start, opts.SegmentRange.End)
			}
			return nil, f
		}
		zeroGap(out, g.StartFrame, g.EndFrame)
	}

	if opts.ExpectedFrames > 0 && out.Frames() != opts.ExpectedFrames {
		return nil, media.NewFault(media.CodeIncompleteRange, "frame count %d != expected %d", out.Frames(), opts.ExpectedFrames).
			WithTrack(trackID).WithRange(opts.SegmentRange.Start, opts.SegmentRange.End)
	}
	return out, nil
}

func zeroGap(p *PCM, start, end int) {
	if start < 0 {
		start = 0
	}
	if end > p.Frames() {
		end = p.Frames()
	}
	for f := start; f < end; f++ {
		for c := 0; c < p.Channels; c++ {
			p.Samples[f*p.Channels+c] = 0
		}
	}
}

// ClippingRun describes a run of consecutive full-scale samples.
type ClippingRun struct {
	StartSample int
	Count       int
}

// DetectClipping finds full-scale runs. A run of at least minConsecutive is
// treated as irreversible clipping and returned as a fault with track context.
func DetectClipping(p *PCM, trackID int64, minConsecutive int, rng media.TimeRange) *media.Fault {
	if p == nil {
		return media.NewFault(media.CodeEmptyChannel, "nil pcm").WithTrack(trackID)
	}
	if minConsecutive <= 0 {
		minConsecutive = 1
	}
	run := 0
	start := 0
	for i, s := range p.Samples {
		if s == math.MaxInt16 || s == math.MinInt16 {
			if run == 0 {
				start = i
			}
			run++
			if run >= minConsecutive {
				return media.NewFault(media.CodeClipping, "clipping at sample %d (run %d)", start, run).
					WithTrack(trackID).WithRange(rng.Start, rng.End)
			}
		} else {
			run = 0
		}
	}
	return nil
}
