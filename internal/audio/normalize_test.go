package audio

import (
	"math"
	"testing"

	"mediad/internal/fault"
)

func sine(frames, rate int, hz, amp float64, channels int) []float64 {
	out := make([]float64, frames*channels)
	for i := 0; i < frames; i++ {
		v := amp * math.Sin(2*math.Pi*hz*float64(i)/float64(rate))
		for ch := 0; ch < channels; ch++ {
			out[i*channels+ch] = v
		}
	}
	return out
}

func TestResampleDurationAndDeterminism(t *testing.T) {
	in := sine(44100, 44100, 440, 0.5, 1)
	out, frames, err := Resample(in, 1, 44100, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if frames != 48000 {
		t.Fatalf("frames = %d", frames)
	}
	out2, _, _ := Resample(in, 1, 44100, 48000)
	for i := range out {
		if out[i] != out2[i] {
			t.Fatalf("resample not deterministic at %d: %v vs %v", i, out[i], out2[i])
		}
	}
	if DurationTicks(out, 48000, 1) != 90000 {
		t.Fatalf("duration ticks = %d", DurationTicks(out, 48000, 1))
	}
}

func TestResampleRejectsBadLayout(t *testing.T) {
	if _, _, err := Resample([]float64{1, 2, 3}, 2, 48000, 48000); err == nil {
		t.Fatal("expected layout error")
	}
	if _, _, err := Resample([]float64{1}, 1, 0, 48000); err == nil {
		t.Fatal("expected rate error")
	}
}

func TestLoudnessApproachesTarget(t *testing.T) {
	// A stereo 1 kHz tone at a known amplitude has a stable loudness; the
	// normaliser must bring it to the target within a tight tolerance.
	in := sine(48000*2, 48000, 1000, 0.05, 2)
	measured := LoudnessLUFS(in, 48000, 2)
	if math.IsInf(measured, -1) {
		t.Fatal("silence detected")
	}
	src := Source{URI: "a.m4s", StartTicks: 0, DurationTicks: 180000, SampleRate: 48000, Channels: 2, Samples: in, Decodable: true}
	res, faults := Normalize(src, DefaultTarget(), 1, "rep")
	if faults.Len() != 0 {
		t.Fatalf("faults: %v", faults.Err())
	}
	if res.Gap {
		t.Fatal("unexpected gap")
	}
	if math.Abs(res.MeasuredLUFS+23) > 8 {
		t.Fatalf("measured loudness %.2f is not near target", res.MeasuredLUFS)
	}
	if math.Abs(res.GainDB) < 1 {
		t.Fatalf("gain %.2f dB should be non-trivial", res.GainDB)
	}
	if res.PeakDBFS > DefaultTarget().PeakCeilingDBFS+0.01 {
		t.Fatalf("peak %.2f exceeds ceiling", res.PeakDBFS)
	}
	if math.Abs(LoudnessLUFS(res.Samples, 48000, 2)-DefaultTarget().LoudnessLUFS) > 1.5 {
		t.Fatalf("post gain loudness %.2f", LoudnessLUFS(res.Samples, 48000, 2))
	}
}

func TestGainRespectsPeakCeiling(t *testing.T) {
	if g := GainForTarget(-40, -23, -1, -1); math.Abs(20*math.Log10(g)) > 1e-9 {
		t.Fatalf("gain should be limited to 0 dB, got %.2f", 20*math.Log10(g))
	}
	if g := GainForTarget(-40, -23, -30, -1); math.Abs(20*math.Log10(g)-17) > 1e-6 {
		t.Fatalf("unlimited gain wrong: %.2f", 20*math.Log10(g))
	}
}

func TestUndecodableLeavesTimelineGap(t *testing.T) {
	src := Source{URI: "bad.m4s", StartTicks: 180000, DurationTicks: 90000, Decodable: false}
	res, faults := Normalize(src, DefaultTarget(), 2, "rep")
	if !res.Gap {
		t.Fatal("expected gap")
	}
	if res.StartTicks != 180000 || res.DurationTicks != 90000 {
		t.Fatalf("timeline shifted: %+v", res)
	}
	if res.Samples != nil {
		t.Fatal("gap must not fabricate samples")
	}
	if faults.ByKind()[fault.KindUndecodableAudio] != 1 {
		t.Fatalf("faults = %v", faults.All())
	}
}

func TestDownmixLayouts(t *testing.T) {
	mono := []float64{1, 1}
	up := UpmixMonoToStereo(mono)
	if len(up) != 4 || math.Abs(up[0]-1/math.Sqrt2) > 1e-9 {
		t.Fatalf("upmix = %v", up)
	}
	six := make([]float64, 6*2)
	six[0], six[1], six[2], six[4], six[5] = 0.5, 0.25, 0.5, 0.2, 0.1
	stereo, err := DownmixToStereo(six, 6)
	if err != nil {
		t.Fatal(err)
	}
	wantL := 0.5 + 0.70710678*0.5 + 0.70710678*0.2
	if math.Abs(stereo[0]-wantL) > 1e-6 {
		t.Fatalf("downmix L = %v want %v", stereo[0], wantL)
	}
	if _, err := DownmixToStereo([]float64{1, 2, 3}, 2); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeAllGapPositions(t *testing.T) {
	good := Source{URI: "a.m4s", StartTicks: 0, DurationTicks: 90000, SampleRate: 48000, Channels: 2, Samples: sine(48000, 48000, 440, 0.1, 2), Decodable: true}
	bad := Source{URI: "b.m4s", StartTicks: 90000, DurationTicks: 90000, Decodable: false}
	results, faults := NormalizeAll([]Source{good, bad}, DefaultTarget())
	if len(results) != 2 || !results[1].Gap {
		t.Fatalf("results = %+v", results)
	}
	found := false
	for _, f := range faults {
		if f.Kind == fault.KindUndecodableAudio && f.Loc.Segment == "b.m4s" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gap fault missing: %+v", faults)
	}
	if results[1].StartTicks != 90000 {
		t.Fatalf("gap start moved: %d", results[1].StartTicks)
	}
}

func TestSampleDurationTicks(t *testing.T) {
	if got := SampleDurationTicks(48000, 48000); got != 90000 {
		t.Fatalf("ticks = %d", got)
	}
	if got := TicksToFrames(90000, 48000); got != 48000 {
		t.Fatalf("frames = %d", got)
	}
	if SampleDurationTicks(0, 48000) != 0 || TicksToFrames(0, 48000) != 0 {
		t.Fatal("zero inputs must yield zero")
	}
}

func TestTargetValidation(t *testing.T) {
	if err := DefaultTarget().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := Target{SampleRate: 44100, Channels: 2, LoudnessLUFS: -23, PeakCeilingDBFS: -1}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected sample rate error")
	}
}
