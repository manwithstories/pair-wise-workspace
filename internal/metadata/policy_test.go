package metadata

import "testing"

func TestGroupAudioSplitsOnRateAndLayout(t *testing.T) {
	stereo := Audio{Channels: 2, SampleRateHz: 48000, SampleRateRaw: 48000 << 16, Layout: "L R"}
	six := Audio{Channels: 6, SampleRateHz: 48000, SampleRateRaw: 48000 << 16, Layout: "L R C LFE Ls Rs"}
	descs := []Descriptor{
		{Representation: "high", Audio: &six},
		{Representation: "mid", Audio: &six},
		{Representation: "low", Audio: &stereo},
	}
	groups := GroupAudio(descs)
	if len(groups) != 2 {
		t.Fatalf("want 2 audio groups, got %d", len(groups))
	}
	for _, g := range groups {
		if len(g.Representations) == 2 && g.Channels != 6 {
			t.Fatalf("6-channel group expected, got %+v", g)
		}
	}
}

func TestCheckVideoMarksOddColorOut(t *testing.T) {
	bt709 := Color{ColorType: "nclx", Primaries: 1, Transfer: 1, Matrix: 1}
	hlg := Color{ColorType: "nclx", Primaries: 9, Transfer: 18, Matrix: 9}
	descs := []Descriptor{
		{Representation: "high", Color: &bt709},
		{Representation: "mid", Color: &bt709},
		{Representation: "low", Color: &hlg},
	}
	rep := CheckVideo(descs)
	if len(rep.Compatible) != 2 || rep.Compatible[0] != "high" || rep.Compatible[1] != "mid" {
		t.Fatalf("compatible=%v want [high mid]", rep.Compatible)
	}
	if len(rep.Incompatible) != 1 || rep.Incompatible[0] != "low" {
		t.Fatalf("incompatible=%v want [low]", rep.Incompatible)
	}
}
