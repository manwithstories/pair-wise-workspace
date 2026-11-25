package audio

import "testing"

func TestMeasureSilenceAndTone(t *testing.T) {
	sil := &PCM{Rate: 48000, Channels: 2, Samples: make([]int16, 960)}
	if r := MeasureLoudness(sil); r.IntegratedLUFS > -119 {
		t.Fatalf("silence should floor near -120, got %+v", r)
	}
	tone := TonePCM(48000, 2, 4096, 25)
	r := MeasureLoudness(tone)
	if r.IntegratedLUFS >= 0 || r.IntegratedLUFS < -60 {
		t.Fatalf("tone loudness implausible: %+v", r)
	}
}

func TestNormalizeToTargetLoudness(t *testing.T) {
	rng := segRange(t)
	tone := TonePCM(48000, 2, 8192, 25)
	out, rep, f := NormalizeLoudness(tone, NormalizeOptions{
		TrackID: 1, TargetLUFS: -18, CeilingDBFS: -1, Range: rng,
	})
	if f != nil {
		t.Fatal(f)
	}
	after := MeasureLoudness(out)
	diff := after.IntegratedLUFS - (-18)
	if diff < 0 {
		diff = -diff
	}
	if diff > 1.0 {
		t.Fatalf("normalized LUFS=%g (rep=%+v) not within 1.0 of -18", after.IntegratedLUFS, rep)
	}
	if rep.Limited {
		t.Fatal("moderate tone should not hit limiter")
	}
}

func TestNormalizeRejectsClippedInput(t *testing.T) {
	rng := segRange(t)
	clip := ClippedPCM(2, 2048, 100, 8)
	_, _, f := NormalizeLoudness(clip, NormalizeOptions{
		TrackID: 3, TargetLUFS: -18, RejectClippedInput: true, ClippingRun: 3, Range: rng,
	})
	if f == nil || f.Code != "clipping" {
		t.Fatalf("want clipping fault, got %v", f)
	}
}

func TestLimiterPreventsFullScale(t *testing.T) {
	rng := segRange(t)
	quiet := TonePCM(48000, 2, 8192, 1) // moderate tone; ask a very loud target to force gain
	out, rep, f := NormalizeLoudness(quiet, NormalizeOptions{
		TrackID: 1, TargetLUFS: -5, CeilingDBFS: -1, Range: rng,
	})
	if f != nil {
		t.Fatal(f)
	}
	if !rep.Limited {
		t.Fatal("expected limiter to engage for loud target")
	}
	if f := DetectClipping(out, 1, 1, rng); f != nil {
		t.Fatalf("limited output must not be full-scale clipped: %v", f)
	}
}
