package gen

import (
	"math"
	"os"
	"path/filepath"
)

// SelftestCase describes one generated fixture and its expected result.
type SelftestCase struct {
	Name               string
	Path               string
	ExpectedIntegrated float64
	ExpectedTruePeak   float64
	ExpectedReason     string
	CheckIntegrated    bool
	CheckTruePeak      bool
	Tolerance          float64
}

// GenerateSelftest writes six deterministic fixtures into dir.
func GenerateSelftest(dir string) ([]SelftestCase, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cases := make([]SelftestCase, 0, 6)

	sinePath := filepath.Join(dir, "sine_1k_stereo_48k.wav")
	sineAmp := sineAmplitude(48000, -23.017, 1000, 1)
	if err := WriteWAV(sinePath, WAVSpec{SampleRate: 48000, Channels: 2, Bits: 16, Seconds: fixtureSeconds},
		func(frame, ch int) float64 {
			if ch == 0 {
				return sineAmp * math.Sin(2*math.Pi*1000*float64(frame)/48000)
			}
			return 0
		}); err != nil {
		return nil, err
	}
	cases = append(cases, SelftestCase{
		Name: "sine_1k_stereo_48k", Path: sinePath,
		ExpectedIntegrated: -23.02, ExpectedTruePeak: -20.01,
		CheckIntegrated: true, CheckTruePeak: true, Tolerance: 0.15,
	})

	phasePath := filepath.Join(dir, "phase_sine_quarter_fs.wav")
	if err := WriteWAV(phasePath, WAVSpec{SampleRate: 48000, Channels: 1, Bits: 32, Float: true, Seconds: 2},
		func(frame, ch int) float64 {
			return math.Sqrt2 * math.Sin(math.Pi*float64(frame)/2+math.Pi/4)
		}); err != nil {
		return nil, err
	}
	cases = append(cases, SelftestCase{
		Name: "phase_sine_quarter_fs", Path: phasePath,
		ExpectedTruePeak: 3.01, CheckTruePeak: true, Tolerance: 0.15,
	})

	monoPath := filepath.Join(dir, "mono_1k_32k.wav")
	monoAmp := sineAmplitude(32000, -23, 1000, 1)
	if err := WriteWAV(monoPath, WAVSpec{SampleRate: 32000, Channels: 1, Bits: 16, Seconds: fixtureSeconds},
		sine(32000, monoAmp, 1000, 0)); err != nil {
		return nil, err
	}
	cases = append(cases, SelftestCase{
		Name: "mono_1k_32k", Path: monoPath,
		ExpectedIntegrated: -23.00, CheckIntegrated: true, Tolerance: 0.08,
	})

	surroundPath := filepath.Join(dir, "surround_51_96k.wav")
	if err := writeSurroundFixture(surroundPath); err != nil {
		return nil, err
	}
	cases = append(cases, SelftestCase{
		Name: "surround_51_96k", Path: surroundPath,
		ExpectedIntegrated: -23.00, CheckIntegrated: true, Tolerance: 0.08,
	})

	silentPath := filepath.Join(dir, "silent_48k.wav")
	if err := WriteWAV(silentPath, WAVSpec{SampleRate: 48000, Channels: 2, Bits: 16, Seconds: fixtureSeconds},
		func(frame, ch int) float64 { return 0 }); err != nil {
		return nil, err
	}
	cases = append(cases, SelftestCase{
		Name: "silent_48k", Path: silentPath, ExpectedReason: "silent",
	})

	shortPath := filepath.Join(dir, "too_short_48k.wav")
	if err := WriteWAV(shortPath, WAVSpec{SampleRate: 48000, Channels: 1, Bits: 16, Seconds: 0.25},
		sine(48000, 0.1, 1000, 0)); err != nil {
		return nil, err
	}
	cases = append(cases, SelftestCase{
		Name: "too_short_48k", Path: shortPath, ExpectedReason: "too_short",
	})
	return cases, nil
}
