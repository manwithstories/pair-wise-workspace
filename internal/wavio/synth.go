package wavio

import (
	"math"
	"os"
	"path/filepath"
)

type RNG struct {
	state uint64
}

func NewRNG(seed uint64) *RNG {
	if seed == 0 {
		seed = 0x9E3779B97F4A7C15
	}
	return &RNG{state: seed}
}

func (r *RNG) Uint64() uint64 {
	r.state ^= r.state >> 12
	r.state ^= r.state << 25
	r.state ^= r.state >> 27
	return r.state * 0x2545F4914F6CDD1D
}

func (r *RNG) Float64() float64 {
	return float64(r.Uint64()>>11) / float64(uint64(1)<<53)
}

func (r *RNG) Normal() float64 {
	u1 := 1-r.Float64()
	u2 := r.Float64()
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

func TonePlanar(sampleRate int, durationSec float64, channels int, freq float64, amplitude float64, phase float64) [][]float64 {
	frames := int(math.Round(float64(sampleRate) * durationSec))
	planar := make([][]float64, channels)
	for ch := range planar {
		planar[ch] = make([]float64, frames)
		channelPhase := phase + float64(ch)*0.37
		for n := 0; n < frames; n++ {
			planar[ch][n] = amplitude * math.Sin(2*math.Pi*freq*float64(n)/float64(sampleRate)+channelPhase)
		}
	}
	return planar
}

// Equal-octave deterministic pink noise, seeded so every fixture is reproducible.
func PinkPlanar(sampleRate int, durationSec float64, channels int, amplitude float64, seed uint64) [][]float64 {
	frames := int(math.Round(float64(sampleRate) * durationSec))
	planar := make([][]float64, channels)
	components := make([]struct {
		freq, phase, amp float64
	}, 0, 64)
	rng := NewRNG(seed)
	for f := 20.0; f <= 18000; f *= math.Pow(2, 1.0/3.0) {
		components = append(components, struct{ freq, phase, amp float64 }{
			freq:  f,
			phase: rng.Float64() * 2 * math.Pi,
			amp:   1 / math.Sqrt(f),
		})
	}
	for ch := range planar {
		channelRNG := NewRNG(seed + uint64(ch+1)*0x1000)
		data := make([]float64, frames)
		var rmsSum float64
		for n := 0; n < frames; n++ {
			var value float64
			t := float64(n) / float64(sampleRate)
			for _, component := range components {
				value += component.amp * math.Sin(2*math.Pi*component.freq*t+component.phase)
			}
			data[n] = value
			rmsSum += value * value
		}
		rms := math.Sqrt(rmsSum / float64(frames))
		scale := amplitude / rms
		for n := range data {
			data[n] *= scale
			data[n] += 0.002 * channelRNG.Normal()
		}
		planar[ch] = data
	}
	return planar
}

func MixInto(base [][]float64, startSample int, burst [][]float64) {
	for ch := range base {
		if ch >= len(burst) {
			continue
		}
		end := startSample + len(burst[ch])
		if end > len(base[ch]) {
			end = len(base[ch])
		}
		for n := startSample; n < end; n++ {
			base[ch][n] += burst[ch][n-startSample]
		}
	}
}

func WriteTruncated(path string, header WriteHeader, planar [][]float64, dropBytes int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	fullPath := path + ".tmp"
	if err := WriteWAV(fullPath, header, planar); err != nil {
		return err
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return err
	}
	if err := os.Remove(fullPath); err != nil {
		return err
	}
	if dropBytes < 0 {
		dropBytes = 0
	}
	if dropBytes > len(data) {
		dropBytes = len(data)
	}
	return os.WriteFile(path, data[:len(data)-dropBytes], 0o644)
}
