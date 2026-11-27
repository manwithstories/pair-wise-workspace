package timeline

import (
	"testing"

	"avcheck/internal/clock"
	"avcheck/internal/fault"
	"avcheck/internal/wav"
)

func seg(rate int, lay wav.Layout, samples []int16) Segment {
	return Segment{Start: 0, PCM: &wav.PCM{Rate: rate, Layout: lay, Samples: samples}}
}

func TestResolveConfigConflictDeterministic(t *testing.T) {
	tracks := []Track{
		{Name: "a", Color: ColorMeta{"bt709", "gamma22"}},
		{Name: "b", Color: ColorMeta{"bt709", "gamma22"}},
		{Name: "c", Color: ColorMeta{"bt2020", "pq"}},
		{Name: "d", Color: ColorMeta{"bt2020", "gamma22"}},
	}
	cfg, err := ResolveConfig(tracks)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Primaries != "bt709" || cfg.Transfer != "gamma22" {
		t.Fatalf("winner %+v", cfg)
	}
	if len(cfg.Conflicts) != 2 {
		t.Fatalf("conflicts %v", cfg.Conflicts)
	}
	// Deterministic across runs.
	again, _ := ResolveConfig(tracks)
	if again.Primaries != cfg.Primaries || again.Transfer != cfg.Transfer ||
		len(again.Conflicts) != len(cfg.Conflicts) {
		t.Fatal("resolution not deterministic")
	}
}

func TestResolveConfigUnknown(t *testing.T) {
	_, err := ResolveConfig([]Track{{Name: "x", Color: ColorMeta{"aces", "gamma22"}}})
	if !fault.IsCode(err, fault.Unsupported) {
		t.Fatalf("want unsupported, got %v", err)
	}
	_, err = ResolveConfig([]Track{{Name: "x", Color: ColorMeta{"bt709", "dolby"}}})
	if !fault.IsCode(err, fault.Unsupported) {
		t.Fatalf("want unsupported, got %v", err)
	}
}

func TestMixResamplesAndPlaces(t *testing.T) {
	// 1 second of constant 1000 at 44.1k mono → 48000 stereo frames.
	src := make([]int16, 44100)
	for i := range src {
		src[i] = 1000
	}
	tr := Track{Name: "t", Segments: []Segment{seg(44100, wav.Mono, src)}}
	out := Mix([]Track{tr}, clock.FromSamples(48000, 48000))
	if len(out) != 96000 {
		t.Fatalf("len %d", len(out))
	}
	// Mono matrix gain 0.7071, single track norm 1.
	mid := out[24000*2] // frame 24000, left
	if mid < 650 || mid > 750 {
		t.Fatalf("amplitude %d", mid)
	}
	if out[24000*2] != out[24000*2+1] {
		t.Fatal("mono must be centred")
	}
}

func TestMixSegmentOffsetAndClamp(t *testing.T) {
	src := []int16{30000, 30000, 30000, 30000}
	s := seg(48000, wav.Stereo, src)
	s.Start = clock.FromSamples(10, 48000)
	out := Mix([]Track{{Name: "a", Segments: []Segment{s}}}, clock.FromSamples(20, 48000))
	if out[0] != 0 || out[10*2] != 30000 {
		t.Fatalf("offset placement wrong: %d %d", out[0], out[10*2])
	}
	// Two full-scale tracks must clamp, not wrap.
	big := make([]int16, 100)
	for i := range big {
		big[i] = 32767
	}
	hot := Track{Name: "h", Segments: []Segment{seg(48000, wav.Stereo, big)}}
	mixed := Mix([]Track{hot, hot}, clock.FromSamples(50, 48000))
	for i, v := range mixed {
		if v < 0 {
			t.Fatalf("frame %d wrapped to %d", i, v)
		}
	}
}
