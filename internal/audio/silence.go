// Package audio detects silence in signed 16-bit little-endian PCM.
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/pairwise/mediapack/internal/timeline"
)

// Config configures window energy and the minimum silence duration.
// MinSilenceMS is the millisecond threshold requested by editors.
type Config struct {
	SampleRate      int
	Channels        int
	WindowMS        int
	MinSilenceMS    int
	EnergyThreshold float64 // mean square sample energy; 0 accepts digital silence
}

// DefaultConfig is 16 kHz mono, 20 ms windows, 80 ms minimum silence.
func DefaultConfig() Config {
	return Config{SampleRate: 16000, Channels: 1, WindowMS: 20, MinSilenceMS: 80, EnergyThreshold: 1}
}

// Silence is a detected silent interval on timeline.Clock's unified axis.
type Silence = timeline.Interval

var ErrInvalidPCM = errors.New("invalid PCM S16LE data")

// Analyze computes per-window mean-square energy and merges silent windows
// lasting at least MinSilenceMS. start is the PTS of the first PCM byte.
func Analyze(start time.Duration, pcm []byte, cfg Config) ([]Silence, error) {
	cfg = withDefaults(cfg)
	if cfg.SampleRate <= 0 || cfg.Channels <= 0 || cfg.WindowMS <= 0 || cfg.MinSilenceMS <= 0 {
		return nil, fmt.Errorf("%w: invalid config %+v", ErrInvalidPCM, cfg)
	}
	blockAlign := cfg.Channels * 2
	if len(pcm)%blockAlign != 0 {
		return nil, fmt.Errorf("%w: byte length %d not aligned to %d-byte frames", ErrInvalidPCM, len(pcm), blockAlign)
	}

	framesPerWindow := cfg.SampleRate * cfg.WindowMS / 1000
	if framesPerWindow <= 0 {
		return nil, fmt.Errorf("%w: window %dms too short for sample rate", ErrInvalidPCM, cfg.WindowMS)
	}
	bytesPerWindow := framesPerWindow * blockAlign
	windowDuration := time.Duration(cfg.WindowMS) * time.Millisecond
	minDuration := time.Duration(cfg.MinSilenceMS) * time.Millisecond

	var out []Silence
	var runStart time.Duration
	var runWindows int
	flush := func(end time.Duration) {
		if runWindows > 0 && end-runStart >= minDuration {
			out = append(out, timeline.Interval{
				Start: runStart, End: end, Reason: "silence_pcm_s16le", Recovered: true,
			})
		}
		runWindows = 0
	}

	for offset, winIdx := 0, 0; offset < len(pcm); offset += bytesPerWindow {
		endByte := min(offset+bytesPerWindow, len(pcm))
		energy, _, err := windowEnergy(pcm[offset:endByte])
		if err != nil {
			return nil, err
		}
		winStart := start + time.Duration(winIdx)*windowDuration

		if energy <= cfg.EnergyThreshold {
			if runWindows == 0 {
				runStart = winStart
			}
			runWindows++
		} else {
			flush(winStart)
		}
		winIdx++
	}
	totalFrames := len(pcm) / blockAlign
	flush(start + time.Duration(totalFrames)*time.Second/time.Duration(cfg.SampleRate))
	return out, nil
}

func withDefaults(cfg Config) Config {
	def := DefaultConfig()
	if cfg.SampleRate == 0 {
		cfg.SampleRate = def.SampleRate
	}
	if cfg.Channels == 0 {
		cfg.Channels = def.Channels
	}
	if cfg.WindowMS == 0 {
		cfg.WindowMS = def.WindowMS
	}
	if cfg.MinSilenceMS == 0 {
		cfg.MinSilenceMS = def.MinSilenceMS
	}
	if cfg.EnergyThreshold == 0 {
		cfg.EnergyThreshold = def.EnergyThreshold
	}
	return cfg
}

func windowEnergy(b []byte) (float64, int, error) {
	if len(b)%2 != 0 {
		return 0, 0, fmt.Errorf("%w: odd S16LE byte count", ErrInvalidPCM)
	}
	n := len(b) / 2
	if n == 0 {
		return 0, 0, nil
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(b[2*i : 2*i+2]))
		f := float64(v)
		sum += f * f
	}
	return sum / float64(n), n, nil
}

// TonePCM is a deterministic test/fixture helper. Amplitude 0 creates silence.
func TonePCM(samples int, amplitude int16) []byte {
	b := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		v := amplitude
		if amplitude > 0 && i%8 < 4 {
			v = amplitude
		} else if amplitude > 0 {
			v = -amplitude
		}
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

// RMS converts mean-square energy to root-mean-square amplitude.
func RMS(energy float64) float64 { return math.Sqrt(energy) }
