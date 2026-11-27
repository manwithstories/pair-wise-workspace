// Package normalize standardises the analysis inputs while always preserving
// the original source specification. Audio is resampled according to its
// channel layout mask into a common planar-float analysis format; colour
// conflicts are recorded with their provenance and flagged ambiguous.
package normalize

import (
	"fmt"

	"github.com/asticode/go-astiav"
)

// AudioSpec describes one audio stream's source format. It is retained
// verbatim alongside any derived analysis data so reporting never loses the
// original sample rate / layout.
type AudioSpec struct {
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	SampleFormat  string `json:"sample_format"`
}

// AnalysisTarget is the uniform format audio is converted to for analysis.
// Planar float keeps one contiguous plane per channel for per-channel RMS.
type AnalysisTarget struct {
	SampleRate int
	Layout     astiav.ChannelLayout
	Format     astiav.SampleFormat
}

// DefaultTarget resamples every stream to 48 kHz planar float. The channel
// layout is taken from the source (downmixed only when explicitly requested),
// so stereo and 5.1 inputs retain their channel count for per-channel stats.
func DefaultTarget() AnalysisTarget {
	return AnalysisTarget{
		SampleRate: 48000,
		Layout:     astiav.ChannelLayoutStereo,
		Format:     astiav.SampleFormatFltp,
	}
}

// AudioNormalizer converts decoded audio frames to the analysis target. It
// owns a single SwrContext and is not safe for concurrent use; the pipeline
// runs one normalizer per worker track.
type AudioNormalizer struct {
	target AnalysisTarget
	swr    *astiav.SoftwareResampleContext
	// Source records the most recent input spec for provenance.
	Source AudioSpec
}

// NewAudioNormalizer builds a normalizer. target.Layout is overridden per
// stream so the source channel mask is preserved.
func NewAudioNormalizer(target AnalysisTarget) *AudioNormalizer {
	return &AudioNormalizer{
		target: target,
		swr:    astiav.AllocSoftwareResampleContext(),
	}
}

// Free releases the resample context.
func (n *AudioNormalizer) Free() {
	if n.swr != nil {
		n.swr.Free()
		n.swr = nil
	}
}

// Resample converts one decoded frame into zero or more target-format frames.
// The output layout follows the source mask; the sample rate and sample
// format follow the analysis target. Callers must Free every returned frame.
func (n *AudioNormalizer) Resample(in *astiav.Frame) ([]*astiav.Frame, error) {
	if n.swr == nil {
		return nil, fmt.Errorf("normalize: normalizer already freed")
	}
	n.Source = AudioSpec{
		SampleRate:   in.SampleRate(),
		Channels:     in.ChannelLayout().Channels(),
		SampleFormat: in.SampleFormat().String(),
	}
	dst := astiav.AllocFrame()
	if dst == nil {
		return nil, fmt.Errorf("normalize: alloc dst frame failed")
	}
	dst.SetChannelLayout(in.ChannelLayout())
	dst.SetSampleRate(n.target.SampleRate)
	dst.SetSampleFormat(n.target.Format)
	// Upper bound on output samples given the rate ratio, plus resampler tail.
	ratio := float64(n.target.SampleRate) / float64(max1(in.SampleRate()))
	dst.SetNbSamples(int(float64(in.NbSamples())*ratio) + 32)
	if err := dst.AllocBuffer(0); err != nil {
		dst.Free()
		return nil, fmt.Errorf("normalize: alloc dst buffer: %w", err)
	}
	if err := n.swr.ConvertFrame(in, dst); err != nil {
		dst.Free()
		return nil, fmt.Errorf("normalize: convert frame: %w", err)
	}
	if dst.NbSamples() <= 0 {
		dst.Free()
		return nil, nil
	}
	return []*astiav.Frame{dst}, nil
}

// Flush drains any samples buffered inside the resampler at stream end.
func (n *AudioNormalizer) Flush() ([]*astiav.Frame, error) {
	if n.swr == nil {
		return nil, nil
	}
	dst := astiav.AllocFrame()
	if dst == nil {
		return nil, fmt.Errorf("normalize: alloc flush frame failed")
	}
	dst.SetChannelLayout(layoutForName(n.Source.ChannelLayout))
	dst.SetSampleRate(n.target.SampleRate)
	dst.SetSampleFormat(n.target.Format)
	dst.SetNbSamples(8192)
	if err := dst.AllocBuffer(0); err != nil {
		dst.Free()
		return nil, err
	}
	if err := n.swr.ConvertFrame(nil, dst); err != nil {
		dst.Free()
		return nil, fmt.Errorf("normalize: flush: %w", err)
	}
	if dst.NbSamples() <= 0 {
		dst.Free()
		return nil, nil
	}
	return []*astiav.Frame{dst}, nil
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

func layoutForName(name string) astiav.ChannelLayout {
	switch name {
	case "mono":
		return astiav.ChannelLayoutMono
	case "5.1":
		return astiav.ChannelLayout5Point1
	default:
		return astiav.ChannelLayoutStereo
	}
}
