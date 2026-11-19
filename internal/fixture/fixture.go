// Package fixture generates every input the pipeline consumes: multichannel
// PCM/WAVE blocks, video frame timestamps, RTP packets and a small local MP4
// diagnostic clip. Nothing is downloaded and no network is touched.
package fixture

import (
	"fmt"
	"math"

	"avpipe/internal/audio"
	"avpipe/internal/rng"
)

// SignalConfig describes a generated test tone.
type SignalConfig struct {
	SampleRate int
	Layout     audio.Layout
	Frames     int
	BlockSize  int
	Amplitude  float64
	ToneHz     float64
	NoiseAmp   float64
	Seed       uint64
	// DropIndex, when >=0, silences exactly one block to exercise silent
	// block handling without making the whole stream silent.
	DropIndex int
}

// Signal returns interleaved PCM blocks plus the matching container bytes.
func Signal(cfg SignalConfig) ([]audio.Block, []byte, audio.Format, error) {
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 48000
	}
	if cfg.Frames == 0 {
		cfg.Frames = cfg.SampleRate // one second
	}
	if cfg.BlockSize == 0 {
		cfg.BlockSize = 1024
	}
	if cfg.Amplitude == 0 {
		cfg.Amplitude = 0.25
	}
	if cfg.ToneHz == 0 {
		cfg.ToneHz = 440
	}
	if cfg.DropIndex == 0 && cfg.Frames > 0 {
		cfg.DropIndex = -1
	}
	ch := len(cfg.Layout.Channels)
	if ch == 0 {
		return nil, nil, audio.Format{}, fmt.Errorf("fixture: empty layout")
	}
	f := audio.Format{SampleRate: cfg.SampleRate, Channels: ch,
		BitsPerSample: 16, Layout: cfg.Layout}
	r := rng.New(cfg.Seed)
	all := make([]float64, cfg.Frames*ch)
	for frame := 0; frame < cfg.Frames; frame++ {
		block := frame / cfg.BlockSize
		base := cfg.Amplitude * math.Sin(2*math.Pi*cfg.ToneHz*float64(frame)/float64(cfg.SampleRate))
		if cfg.DropIndex >= 0 && block == cfg.DropIndex {
			base = 0
		}
		for c := 0; c < ch; c++ {
			// Slight per-channel phase/level difference so mapping is visible.
			v := base * (1 - 0.1*float64(c))
			if cfg.NoiseAmp > 0 {
				v += cfg.NoiseAmp * r.Symmetric()
			}
			all[frame*ch+c] = v
		}
	}
	data, err := audio.Encode(f, all, cfg.Frames)
	if err != nil {
		return nil, nil, audio.Format{}, err
	}
	info, err := audio.Parse(data, cfg.Layout.Name)
	if err != nil {
		return nil, nil, audio.Format{}, err
	}
	blocks, err := audio.Decode(data, info, cfg.BlockSize)
	if err != nil {
		return nil, nil, audio.Format{}, err
	}
	return blocks, data, f, nil
}

// CorruptWAV truncates the image just inside the data payload while leaving
// the declared chunk length intact, which is the truncation the parser must
// localize.
func CorruptWAV(data []byte) []byte {
	if len(data) < 60 {
		return data
	}
	out := append([]byte(nil), data[:len(data)-len(data)/5]...)
	return out
}

// Frame is one synthetic video frame timestamp.
type Frame struct {
	Index int
	Tick  int64
	Key   bool
}

// VideoFrames generates frame timestamps at fps on the 90 kHz grid, with an
// optional drift in ppm and periodic keyframes.
func VideoFrames(count, fps int, startTick int64, driftPPM float64, keyEvery int) []Frame {
	if fps <= 0 {
		fps = 30
	}
	out := make([]Frame, count)
	for i := 0; i < count; i++ {
		base := float64(startTick) + float64(i)*90000/float64(fps)
		drift := base * driftPPM / 1e6
		out[i] = Frame{Index: i, Tick: int64(base + drift),
			Key: keyEvery > 0 && i%keyEvery == 0}
	}
	return out
}
