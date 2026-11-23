// Package pipeline orchestrates the delivery stages: sample generation and
// fault injection, stream parsing, clock repair, index rebuild, audio/color
// checks, fan-out/fan-in, and metric reporting.
package pipeline

import (
	"fmt"
	"math/rand"

	"mediad/internal/audio"
	"mediad/internal/clock"
	"mediad/internal/color"
)

// Limits bound memory growth: the working set scales with the segment
// window, input length and representation count.
const (
	MaxDurationSec = 600
	MaxRates       = 8
	MaxLossBurst   = 64
	fps            = 25
	gop            = 25 // one keyframe per second
	frameTicks     = clock.Timescale / fps
)

// Config selects the sample and injected faults.
type Config struct {
	Seed           int64
	DurationSec    int
	LossBurst      int
	Rates          int
	InjectTruncate bool // cut the elementary stream mid-NAL
	BadAudio       bool // wrong channel map and sample rate
	Unrecoverable  bool // sender buffer drops lost packets (corrupted sample)
}

// Validate rejects out-of-bounds parameters.
func (c Config) Validate() error {
	if c.DurationSec < 1 || c.DurationSec > MaxDurationSec {
		return fmt.Errorf("duration must be 1..%d s", MaxDurationSec)
	}
	if c.Rates < 3 || c.Rates > MaxRates {
		return fmt.Errorf("rates must be 3..%d", MaxRates)
	}
	if c.LossBurst < 0 || c.LossBurst > MaxLossBurst {
		return fmt.Errorf("loss-burst must be 0..%d", MaxLossBurst)
	}
	return nil
}

// Frame is one generated access unit with its raw wire timestamp.
type Frame struct {
	Payload []byte // Annex-B bytes for this AU
	TS      uint32 // raw 32-bit 90 kHz timestamp
	Key     bool
}

// Source is a deterministically generated multi-camera sample.
type Source struct {
	Stream         []byte // concatenated elementary stream
	Frames         []Frame
	Trusted        map[uint64]bool // trusted-anchor sequence numbers
	Audio          audio.Track
	CameraColor    color.Metadata
	BitstreamColor color.Metadata
	Truncated      bool
}

// nal builds one Annex-B NAL unit ending with the rbsp stop bit.
func nal(typ byte, rng *rand.Rand, size int) []byte {
	b := []byte{0, 0, 0, 1, typ}
	for i := 0; i < size; i++ {
		b = append(b, byte(rng.Intn(256)))
	}
	return append(b, 0x80)
}

// Generate builds the sample deterministically from cfg.Seed. The timestamp
// base is placed so the 32-bit clock wraps one third in; an encoder restart
// (with a trusted anchor) occurs two thirds in.
func Generate(cfg Config) *Source {
	rng := rand.New(rand.NewSource(cfg.Seed))
	n := cfg.DurationSec * fps
	src := &Source{Trusted: map[uint64]bool{}}

	wrapAt := n / 3
	restartAt := 2 * n / 3
	base := uint32(uint64(clock.WrapModulus) - uint64(wrapAt)*uint64(frameTicks))

	for i := 0; i < n; i++ {
		key := i%gop == 0
		var ts uint32
		if i < restartAt {
			ts = base + uint32(i)*uint32(frameTicks)
		} else {
			// Encoder restart: clock resets to a small value.
			ts = uint32(1000) + uint32(i-restartAt)*uint32(frameTicks)
			if i == restartAt {
				src.Trusted[uint64(i)] = true
			}
		}
		var au []byte
		au = append(au, nal(9, rng, 1)...) // AUD
		if key {
			au = append(au, nal(7, rng, 8)...)   // SPS
			au = append(au, nal(5, rng, 3200)...) // IDR
		} else {
			au = append(au, nal(1, rng, 1100+rng.Intn(300))...) // non-IDR
		}
		src.Frames = append(src.Frames, Frame{Payload: au, TS: ts, Key: key})
		src.Stream = append(src.Stream, au...)
	}

	if cfg.InjectTruncate && len(src.Stream) > 64 {
		src.Stream = src.Stream[:len(src.Stream)-64-3] // cut mid-NAL
		src.Truncated = true
	}

	src.Audio = audio.Track{
		LayoutName: "5.1", SampleRate: 48000,
		Channels:       []string{"FL", "FR", "FC", "LFE", "SL", "SR"},
		PrimingSamples: 512,
	}
	if cfg.BadAudio {
		src.Audio.Channels = append(src.Audio.Channels, "WIBBLE")
		src.Audio.SampleRate = 44100
	}
	src.CameraColor = color.Metadata{
		Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "limited", Source: "camera-config",
	}
	src.BitstreamColor = color.Metadata{
		Primaries: "bt2020", Transfer: "pq", Matrix: "bt2020nc", Range: "full", Source: "bitstream",
	}
	return src
}
