package ingest

import (
	"math"
	"time"
)

// tonePlanes builds one deterministic planar float32 frame, the layout the
// AAC encoder consumes.
func tonePlanes(samples, pos int, seed int64) [][]float32 {
	freq := 220 + float64(seed%400)
	planes := make([][]float32, AudioChannels)
	for c := range planes {
		planes[c] = make([]float32, samples)
	}
	for i := 0; i < samples; i++ {
		t := float64(pos+i) / AudioSampleRate
		v := float32(0.17 * math.Sin(2*math.Pi*freq*t))
		for c := 0; c < AudioChannels; c++ {
			planes[c][i] = v
		}
	}
	return planes
}

// toneFrame builds one deterministic stereo sine frame.
func toneFrame(samples, pos int, seed int64) []int16 {
	freq := 220 + float64(seed%400)
	pcm := make([]int16, samples*AudioChannels)
	for i := 0; i < samples; i++ {
		t := float64(pos+i) / AudioSampleRate
		v := int16(5400 * math.Sin(2*math.Pi*freq*t))
		for c := 0; c < AudioChannels; c++ {
			pcm[i*AudioChannels+c] = v
		}
	}
	return pcm
}

// SilenceFrame is the PCM placeholder used when neither FEC nor PLC can fill
// a lost slot.  It is encoded like any other frame so the placeholder is a
// valid Opus packet rather than a hole.
func SilenceFrame(samples int) []int16 {
	return make([]int16, samples*AudioChannels)
}

func opusFrameSamples(cfg Config, i int) int {
	if cfg.OpusDurationProfile != "mixed" {
		return 960
	}
	switch i % 3 {
	case 1:
		return 480 // 10 ms
	case 2:
		return 2880 // 60 ms
	default:
		return 960 // 20 ms
	}
}

func audioSpanSamples(cfg Config) int {
	span := time.Duration(cfg.VideoFrames) * time.Second * time.Duration(VideoFPSDen) / time.Duration(VideoFPSNum)
	return int(span * AudioSampleRate / time.Second)
}

// encodeVideoTrack encodes a moving luma ramp as H.264 with a fixed GOP so
// keyframe intervals are exactly reproducible.
