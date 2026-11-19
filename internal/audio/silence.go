// Package audio detects silence in generated PCM sample data. Silence runs
// spanning chunk boundaries are merged; short gaps inside speech are not.
package audio

import (
	"math"

	"mediasync/internal/media"
)

// Limits bound memory and work.
const (
	MaxSamples = 32 << 20
	MaxChunk   = 48000
)

// Format describes the PCM layout.
type Format struct {
	SampleRate int `json:"sample_rate"`
	Channels   int `json:"channels"`
}

// DefaultFormat is mono 48 kHz 16-bit.
func DefaultFormat() Format { return Format{SampleRate: 48000, Channels: 1} }

// Validate checks the format.
func (f Format) Validate() error {
	if f.SampleRate <= 0 || f.SampleRate > 384_000 {
		return media.New(media.StageAudio, media.InputPCM, media.CodeInvalidArgument,
			"sample rate %d is outside 1..384000", f.SampleRate)
	}
	if f.Channels <= 0 || f.Channels > 8 {
		return media.New(media.StageAudio, media.InputPCM, media.CodeInvalidArgument,
			"channel count %d is outside 1..8", f.Channels)
	}
	return nil
}

// Options control what counts as silence.
type Options struct {
	// ThresholdDBFS is the level at or below which a frame is silent.
	ThresholdDBFS float64
	// MinSilenceMS is the shortest run that is reported.
	MinSilenceMS int
	// MinGapMS is the shortest loud run that splits two silence runs.
	MinGapMS int
	// ChunkSamples is how many frames (all channels) are analysed per chunk.
	ChunkSamples int
}

// DefaultOptions are tuned for programme audio with room tone.
func DefaultOptions() Options {
	return Options{ThresholdDBFS: -45, MinSilenceMS: 300, MinGapMS: 120, ChunkSamples: 1024}
}

func (o Options) normalize() Options {
	if o.ThresholdDBFS == 0 {
		o.ThresholdDBFS = -45
	}
	if o.ThresholdDBFS > 0 {
		o.ThresholdDBFS = 0
	}
	if o.MinSilenceMS <= 0 {
		o.MinSilenceMS = 300
	}
	if o.MinGapMS <= 0 {
		o.MinGapMS = 120
	}
	if o.ChunkSamples <= 0 {
		o.ChunkSamples = 1024
	}
	if o.ChunkSamples > MaxChunk {
		o.ChunkSamples = MaxChunk
	}
	return o
}

// Run is one detected silence interval.
type Run struct {
	StartSample int64   `json:"start_sample"`
	EndSample   int64   `json:"end_sample"`
	StartNS     int64   `json:"start_ns"`
	EndNS       int64   `json:"end_ns"`
	PeakDBFS    float64 `json:"peak_dbfs"`
	Merged      int     `json:"merged_chunks"`
}

// DurationMS returns the run length in whole milliseconds.
func (r Run) DurationMS() int64 { return (r.EndNS - r.StartNS) / 1_000_000 }

// Result is the silence analysis outcome.
type Result struct {
	Runs         []Run          `json:"runs"`
	Issues       []*media.Issue `json:"issues"`
	Frames       int64          `json:"frames"`
	SilentFrames int64          `json:"silent_frames"`
	Chunks       int            `json:"chunks"`
	MergedChunks int            `json:"merged_chunks"`
	SplitGaps    int            `json:"split_gaps"`
}

// SilentRatio returns the silent fraction of analysed frames.
func (r *Result) SilentRatio() float64 {
	if r.Frames == 0 {
		return 0
	}
	return float64(r.SilentFrames) / float64(r.Frames)
}

// Amplitude helper.
func dbfs(peak float64) float64 {
	if peak <= 0 {
		return -120
	}
	v := 20 * math.Log10(peak)
	if v < -120 {
		return -120
	}
	return v
}
