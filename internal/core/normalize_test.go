package core

import (
	"math"
	"testing"

	"mediacheck/internal/adapter/audio"
	"mediacheck/internal/model"
)

func TestNormalizeTrackFullPipeline(t *testing.T) {
	// 44.1k 立体声 → 48k 立体声，1 秒素材。
	frames := make([]audio.Frame, 44100)
	for i := range frames {
		v := int32(5000 * math.Sin(2*math.Pi*440*float64(i)/44100))
		frames[i] = audio.Frame{Samples: []int32{v, v}}
	}
	cfg := NormalizeConfig{
		InRate: 44100, TargetRate: 48000,
		InLayout: model.LayoutStereo, TargetLayout: model.LayoutStereo,
		TargetLUFS: -23, PeakLimit: 0.95, FillSilence: true, TrackID: 3,
	}
	res, err := NormalizeTrack(cfg, frames)
	if err != nil {
		t.Fatal(err)
	}
	// 有理数核算期望 48000，实际允许 ±1 帧（规格硬性要求）。
	if res.ExpectedOut != 48000 {
		t.Fatalf("expected out = %d, want 48000", res.ExpectedOut)
	}
	if res.FrameDelta > 1 {
		t.Fatalf("frames out = %d, delta %d exceeds 1 sample", res.FramesOut, res.FrameDelta)
	}
	if res.SamplesIn != int64(44100*2) || res.SamplesOut != int64(res.FramesOut)*2 {
		t.Fatalf("sample counts in=%d out=%d", res.SamplesIn, res.SamplesOut)
	}
	if len(res.PCM16) != res.FramesOut*2*2 {
		t.Fatalf("pcm bytes = %d", len(res.PCM16))
	}
	if math.IsNaN(res.Stats.IntegratedLUFS) || math.IsNaN(res.PeakDBFS) {
		t.Fatal("metrics must be finite")
	}
}

func TestNormalizeTrack51ToStereo(t *testing.T) {
	frames := make([]audio.Frame, 4800) // 0.1s
	for i := range frames {
		frames[i] = audio.Frame{Samples: []int32{1000, 1000, 800, 100, 600, 600}}
	}
	cfg := NormalizeConfig{
		InRate: 48000, TargetRate: 48000,
		InLayout: model.Layout51, TargetLayout: model.LayoutStereo,
		TargetLUFS: -23, PeakLimit: 0.95, FillSilence: true,
	}
	res, err := NormalizeTrack(cfg, frames)
	if err != nil {
		t.Fatal(err)
	}
	if res.OutLayout != model.LayoutStereo || res.FramesOut != 4800 {
		t.Fatalf("downmix result = %+v", res)
	}
	if res.SamplesOut != 4800*2 {
		t.Fatalf("stereo samples = %d", res.SamplesOut)
	}
}

func TestNormalizeTrackRejectsUnknownLayout(t *testing.T) {
	cfg := NormalizeConfig{
		InRate: 48000, TargetRate: 48000,
		InLayout: "atmos", TargetLayout: model.LayoutStereo, TrackID: 9,
	}
	_, err := NormalizeTrack(cfg, []audio.Frame{{Samples: []int32{1, 2}}})
	me, ok := model.AsMediaError(err)
	if !ok || me.Category != model.ErrUnknownLayout || me.TrackID != 9 {
		t.Fatalf("want unknown_layout with track id, got %v", err)
	}
}

func TestNormalizeTrackRejectsBadRate(t *testing.T) {
	cfg := NormalizeConfig{
		InRate: 96000, TargetRate: 48000,
		InLayout: model.LayoutStereo, TargetLayout: model.LayoutStereo,
	}
	_, err := NormalizeTrack(cfg, []audio.Frame{{Samples: []int32{1, 2}}})
	me, ok := model.AsMediaError(err)
	if !ok || me.Category != model.ErrUnsupportedRate {
		t.Fatalf("want unsupported_sample_rate, got %v", err)
	}
}
