package loudness_test

import (
	"errors"
	"math"
	"path/filepath"
	"testing"

	"loudness/internal/gen"
	"loudness/internal/kweight"
	"loudness/internal/loudness"
	"loudness/internal/riff"
)

func TestGeneratedSelftestFixtures(t *testing.T) {
	cases, err := gen.GenerateSelftest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := loudness.NewProcessor()
	for _, tc := range cases {
		m, err := p.ProcessFile(tc.Path)
		if err != nil {
			t.Fatalf("%s: %v", tc.Name, err)
		}
		t.Logf("%s integrated=%.4f short=%.4f momentary=%.4f tp=%.4f blocks=%d reason=%s",
			tc.Name, m.IntegratedLUFS, m.ShortTermMaxLUFS, m.MomentaryMaxLUFS,
			m.TruePeakDBTP, m.GatedBlocks, m.Reason)
		d, err := riff.Open(tc.Path)
		if err == nil {
			rawPeak := 0.0
			buf := make([]float64, 8192*d.Format().Channels)
			for {
				n, e := d.ReadFrames(buf, 8192)
				for _, v := range buf[:n*d.Format().Channels] {
					rawPeak = math.Max(rawPeak, math.Abs(v))
				}
				if e != nil {
					break
				}
			}
			d.Close()
			t.Logf("%s raw peak=%.6f (%.3f dBFS)", tc.Name, rawPeak, 20*math.Log10(rawPeak))
		}
		if tc.ExpectedReason != "" && m.Reason != tc.ExpectedReason {
			t.Errorf("%s reason=%s, want %s", tc.Name, m.Reason, tc.ExpectedReason)
		}
		if tc.CheckIntegrated && math.Abs(m.IntegratedLUFS-tc.ExpectedIntegrated) > tc.Tolerance {
			t.Errorf("%s integrated=%.3f, want %.2f +/- %.2f", tc.Name, m.IntegratedLUFS, tc.ExpectedIntegrated, tc.Tolerance)
		}
		if tc.CheckTruePeak && math.Abs(m.TruePeakDBTP-tc.ExpectedTruePeak) > tc.Tolerance {
			t.Errorf("%s true peak=%.3f, want %.2f +/- %.2f", tc.Name, m.TruePeakDBTP, tc.ExpectedTruePeak, tc.Tolerance)
		}
	}
}

func TestSupportedSampleRateBoundaries(t *testing.T) {
	dir := t.TempDir()
	processor := loudness.NewProcessor()
	for _, fs := range []int{32000, 44100, 48000, 96000, 192000} {
		path := filepath.Join(dir, "rate.wav")
		if err := gen.WriteWAV(path, gen.WAVSpec{SampleRate: fs, Channels: 1, Bits: 16, Seconds: 1},
			func(frame, ch int) float64 { return 0.1 * math.Sin(2*math.Pi*1000*float64(frame)/float64(fs)) }); err != nil {
			t.Fatal(err)
		}
		m, err := processor.ProcessFile(path)
		if err != nil {
			t.Fatalf("%d Hz: %v", fs, err)
		}
		if math.IsInf(m.IntegratedLUFS, 0) || m.GatedBlocks == 0 {
			t.Fatalf("%d Hz metrics = %+v", fs, m)
		}
	}
}

func TestUnsupportedSampleRate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rate.wav")
	if err := gen.WriteWAV(path, gen.WAVSpec{SampleRate: 192001, Channels: 1, Bits: 16, Seconds: 1},
		func(frame, ch int) float64 { return 0 }); err != nil {
		t.Fatal(err)
	}
	if _, err := loudness.NewProcessor().ProcessFile(path); !errors.Is(err, kweight.ErrUnsupportedSampleRate) {
		t.Fatalf("error = %v", err)
	}
}
