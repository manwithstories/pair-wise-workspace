// Package gen creates deterministic WAV fixtures for tests and demos.
package gen

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"loudness/internal/kweight"
)

const fixtureSeconds = 12.0

// WriteSuite writes the eight demo fixtures. The quiet and peak files are
// intentionally non-compliant and corrupt.wav intentionally fails decoding.
func WriteSuite(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	specSine := WAVSpec{SampleRate: 48000, Channels: 2, Bits: 16, Seconds: fixtureSeconds}
	sineAmp := sineAmplitude(48000, -23.017, 1000, 1)
	if err := WriteWAV(filepath.Join(dir, "sine_1k_stereo_48k.wav"), specSine,
		func(frame, ch int) float64 {
			if ch != 0 {
				return 0
			}
			return sineAmp * math.Sin(2*math.Pi*1000*float64(frame)/48000)
		}); err != nil {
		return nil, err
	}

	if err := writePinkFixture(filepath.Join(dir, "pink_noise_stereo_48k.wav")); err != nil {
		return nil, err
	}

	monoAmp := sineAmplitude(32000, -23, 1000, 1)
	if err := WriteWAV(filepath.Join(dir, "mono_1k_32k.wav"),
		WAVSpec{SampleRate: 32000, Channels: 1, Bits: 16, Seconds: fixtureSeconds},
		sine(32000, monoAmp, 1000, 0)); err != nil {
		return nil, err
	}

	if err := writeSurroundFixture(filepath.Join(dir, "surround_51_96k.wav")); err != nil {
		return nil, err
	}

	phaseAmp := sineAmplitude(48000, -23, 12000, 1)
	if err := WriteWAV(filepath.Join(dir, "phase_sine_48k.wav"),
		WAVSpec{SampleRate: 48000, Channels: 1, Bits: 16, Seconds: fixtureSeconds},
		phaseSine(48000, phaseAmp)); err != nil {
		return nil, err
	}

	if err := writeQuietLeadIn(filepath.Join(dir, "quiet_lead_in.wav")); err != nil {
		return nil, err
	}
	if err := writePeakFail(filepath.Join(dir, "peak_fail_48k.wav")); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt.wav"), []byte("not a wave file"), 0o644); err != nil {
		return nil, err
	}
	return []string{
		"corrupt.wav", "mono_1k_32k.wav", "peak_fail_48k.wav", "phase_sine_48k.wav",
		"pink_noise_stereo_48k.wav", "quiet_lead_in.wav", "sine_1k_stereo_48k.wav",
		"surround_51_96k.wav",
	}, nil
}

func writePinkFixture(path string) error {
	const fs = 48000
	frames := int(fs * fixtureSeconds)
	raw := [][]float64{pinkNoise(frames, 11), pinkNoise(frames, 29)}
	gains := []float64{1, 1}
	scale, err := calibrationScale(raw, fs, gains, -23)
	if err != nil {
		return err
	}
	peak := 0.0
	for _, ch := range raw {
		for _, v := range ch {
			peak = math.Max(peak, math.Abs(v*scale))
		}
	}
	if 20*math.Log10(peak) > -1 {
		return fmt.Errorf("pink fixture peak %.2f dBFS", 20*math.Log10(peak))
	}
	return WriteWAV(path, WAVSpec{SampleRate: fs, Channels: 2, Bits: 32, Float: true, Seconds: fixtureSeconds},
		func(frame, ch int) float64 { return raw[ch][frame] * scale })
}

func writeSurroundFixture(path string) error {
	const fs = 96000
	sumGains := 1.0 + 1.0 + 1.0 + 1.41 + 1.41 // LFE is excluded
	amp := sineAmplitude(fs, -23, 1000, sumGains)
	return WriteWAV(path, WAVSpec{
		SampleRate: fs, Channels: 6, Bits: 32, Float: true,
		Extensible: true, ChannelMask: 0x3f, Seconds: 4,
	}, func(frame, ch int) float64 {
		if ch == 3 {
			return 0
		}
		return amp * math.Sin(2*math.Pi*1000*float64(frame)/float64(fs)+float64(ch)*0.2)
	})
}

func writeQuietLeadIn(path string) error {
	const fs = 48000
	quiet := sineAmplitude(fs, -55, 1000, 2)
	body := sineAmplitude(fs, -35, 1000, 2)
	return WriteWAV(path, WAVSpec{SampleRate: fs, Channels: 2, Bits: 16, Seconds: fixtureSeconds},
		func(frame, ch int) float64 {
			amp := body
			if frame < 2*fs {
				amp = quiet
			}
			return amp * math.Sin(2*math.Pi*1000*float64(frame)/float64(fs))
		})
}

func writePeakFail(path string) error {
	const fs = 48000
	amp := sineAmplitude(fs, -23, 1000, 1)
	return WriteWAV(path, WAVSpec{SampleRate: fs, Channels: 1, Bits: 16, Seconds: fixtureSeconds},
		func(frame, ch int) float64 {
			if frame == 5*fs {
				return 0.9
			}
			return amp * math.Sin(2*math.Pi*1000*float64(frame)/float64(fs))
		})
}

func sine(fs int, amp, freq, phase float64) SampleFunc {
	return func(frame, ch int) float64 {
		return amp * math.Sin(2*math.Pi*freq*float64(frame)/float64(fs)+phase)
	}
}

func phaseSine(fs int, amp float64) SampleFunc {
	return func(frame, ch int) float64 {
		return amp * math.Sin(math.Pi*float64(frame)/2+math.Pi/4)
	}
}

func sineAmplitude(fs int, targetLUFS, freq, sumGains float64) float64 {
	d, err := kweight.ForRate(fs)
	if err != nil {
		return 0
	}
	mag := d.MagnitudeAt(fs, freq)
	z := math.Pow(10, (targetLUFS+0.691)/10)
	return math.Sqrt(2 * z / (sumGains * mag * mag))
}

func calibrationScale(raw [][]float64, fs int, gains []float64, targetLUFS float64) (float64, error) {
	if len(raw) == 0 || len(raw) != len(gains) {
		return 0, fmt.Errorf("invalid calibration channels")
	}
	d, err := kweight.ForRate(fs)
	if err != nil {
		return 0, err
	}
	bank, err := kweight.NewBank(d, len(raw))
	if err != nil {
		return 0, err
	}
	frames := len(raw[0])
	interleaved := make([]float64, frames*len(raw))
	weighted := make([]float64, len(interleaved))
	for ch := range raw {
		for i, v := range raw[ch] {
			interleaved[i*len(raw)+ch] = v
		}
	}
	if err := bank.Process(weighted, interleaved, frames); err != nil {
		return 0, err
	}
	energy := 0.0
	for ch := range raw {
		sum := 0.0
		for i := 0; i < frames; i++ {
			v := weighted[i*len(raw)+ch]
			sum += v * v
		}
		energy += gains[ch] * sum / float64(frames)
	}
	if energy <= 0 {
		return 0, fmt.Errorf("zero calibration energy")
	}
	want := math.Pow(10, (targetLUFS+0.691)/10)
	return math.Sqrt(want / energy), nil
}

func pinkNoise(frames int, seed int64) []float64 {
	rng := rand.New(rand.NewSource(seed))
	var b0, b1, b2, b3, b4, b5, b6 float64
	out := make([]float64, frames)
	for i := range out {
		white := rng.NormFloat64()
		b0 = 0.99886*b0 + white*0.0555179
		b1 = 0.99332*b1 + white*0.0750759
		b2 = 0.96900*b2 + white*0.1538520
		b3 = 0.86650*b3 + white*0.3104856
		b4 = 0.55000*b4 + white*0.5329522
		b5 = -0.7616*b5 - white*0.0168980
		out[i] = b0 + b1 + b2 + b3 + b4 + b5 + b6 + white*0.5362
		b6 = white * 0.115926
	}
	return out
}
