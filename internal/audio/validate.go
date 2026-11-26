package audio

import (
	"math"

	"pairflow/internal/media"
)

// ValidateStream checks that a decoded stream has the canonical format and a
// frame count matching the expected timeline length. It is the first gate the
// independent verifier applies to committed audio.
func ValidateStream(pcm []int16, f Format, expectFrames int64, path string) *media.Error {
	if f.Rate != int(media.AudioRate) {
		return media.Integrity("audio.wav_rate", "sample rate %d Hz, want %d", f.Rate, media.AudioRate).WithPath(path)
	}
	if f.Channels != media.AudioChannels {
		return media.Integrity("audio.wav_channels", "channel count %d, want %d", f.Channels, media.AudioChannels).WithPath(path)
	}
	if len(pcm)%f.Channels != 0 {
		return media.Integrity("audio.partial_frame", "sample count %d is not frame aligned", len(pcm)).WithPath(path)
	}
	if got := int64(len(pcm) / f.Channels); got != expectFrames {
		return media.Integrity("audio.frame_count", "%d sample frames, want %d", got, expectFrames).WithPath(path)
	}
	if PeakAbs(pcm) == 0 {
		return media.Integrity("audio.silent", "mix is entirely silent").WithPath(path)
	}
	return nil
}

// SilenceRatio reports the fraction of canonical frames whose peak is zero,
// used to correlate an audio gap with the recovered transport stream.
func SilenceRatio(pcm []int16, channels int) float64 {
	frames := len(pcm) / channels
	if frames == 0 {
		return 0
	}
	silent := 0
	for i := 0; i < frames; i++ {
		peak := 0
		for ch := 0; ch < channels; ch++ {
			v := int(pcm[i*channels+ch])
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		if peak == 0 {
			silent++
		}
	}
	return float64(silent) / float64(frames)
}

// FirstSampleFrame returns the canonical frame index of the first non-zero
// sample across all channels, or -1 when the whole buffer is silent.
func FirstSampleFrame(pcm []int16, channels int) int64 {
	frames := len(pcm) / channels
	for i := 0; i < frames; i++ {
		for ch := 0; ch < channels; ch++ {
			if pcm[i*channels+ch] != 0 {
				return int64(i)
			}
		}
	}
	return -1
}

// LoudnessEnvelope returns the per-block RMS envelope of the mix, used by the
// verifier to confirm the audio program rises and falls with the timeline.
func LoudnessEnvelope(pcm []int16, channels, blockFrames int) []float64 {
	frames := len(pcm) / channels
	blocks := (frames + blockFrames - 1) / blockFrames
	out := make([]float64, blocks)
	for b := 0; b < blocks; b++ {
		start := b * blockFrames
		end := start + blockFrames
		if end > frames {
			end = frames
		}
		var sum float64
		for i := start; i < end; i++ {
			for ch := 0; ch < channels; ch++ {
				v := float64(pcm[i*channels+ch])
				sum += v * v
			}
		}
		n := float64((end - start) * channels)
		if n > 0 {
			out[b] = math.Sqrt(sum / n)
		}
	}
	return out
}
