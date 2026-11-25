package silence

import (
	"math"
	"testing"
)

func TestWindowGeometryIs48kStereo20ms(t *testing.T) {
	if WindowFrames != 960 || WindowBytes != 3840 || WindowNS != 20_000_000 {
		t.Fatalf("bad geometry frames=%d bytes=%d ns=%d", WindowFrames, WindowBytes, WindowNS)
	}
	if MinQuietWindows != 25 || MinSilenceNS != 500_000_000 {
		t.Fatalf("bad sustained threshold windows=%d ns=%d", MinQuietWindows, MinSilenceNS)
	}
}

func TestLoudToneIsNotSilent(t *testing.T) {
	pcm := TonePCM(2, 12000)
	res := DetectS16LE(pcm, nil)
	if len(res.Windows) != 2 {
		t.Fatalf("windows=%d", len(res.Windows))
	}
	for _, w := range res.Windows {
		if w.Silent || w.DBFS <= ThresholdDBFS {
			t.Fatalf("loud window misclassified: %+v", w)
		}
	}
}

func TestSilenceRequires500ms(t *testing.T) {
	quiet24 := TonePCM(24, 0)
	if res := DetectS16LE(quiet24, nil); len(res.Silence) != 0 || res.SilenceNS != 0 {
		t.Fatalf("24 windows must not count as sustained silence: %+v", res.Silence)
	}

	quiet25 := TonePCM(25, 0)
	res := DetectS16LE(quiet25, nil)
	if len(res.Silence) != 1 {
		t.Fatalf("silence spans=%d want 1", len(res.Silence))
	}
	if res.SilenceNS != MinSilenceNS {
		t.Fatalf("silence ns=%d want %d", res.SilenceNS, MinSilenceNS)
	}
	if got := res.Silence[0].EndNS - res.Silence[0].StartNS; got != 500_000_000 {
		t.Fatalf("span duration=%d", got)
	}
}

func TestThresholdIsMinus48DBFS(t *testing.T) {
	// Square wave at amplitude 120 has RMS 120 and is below -48 dBFS.
	pcm := TonePCM(1, 120)
	res := DetectS16LE(pcm, nil)
	if !res.Windows[0].Silent {
		t.Fatalf("120 amplitude should be below threshold, db=%.4f", res.Windows[0].DBFS)
	}
	if math.Abs(res.Windows[0].DBFS-(-48.72)) > 0.05 {
		t.Fatalf("db=%.4f want about -48.72", res.Windows[0].DBFS)
	}

	louder := TonePCM(1, 140)
	res = DetectS16LE(louder, nil)
	if res.Windows[0].Silent {
		t.Fatalf("140 amplitude should be above threshold, db=%.4f", res.Windows[0].DBFS)
	}
}

func TestMissingSamplesAreUnknownAndBreakSilence(t *testing.T) {
	pcm := TonePCM(30, 0)
	gap := Gap{StartNS: 10 * WindowNS, EndNS: 11 * WindowNS}
	res := DetectS16LE(pcm, []Gap{gap})
	if res.UnknownWindows != 1 || len(res.Unknown) != 1 {
		t.Fatalf("unknown windows=%d spans=%+v", res.UnknownWindows, res.Unknown)
	}
	if res.UnknownNS != WindowNS {
		t.Fatalf("unknown ns=%d want %d", res.UnknownNS, WindowNS)
	}
	if len(res.Silence) != 0 {
		t.Fatalf("unknown gap must break sustained silence, got %+v", res.Silence)
	}
	for _, w := range res.Windows {
		if w.Index == 10 && (!w.Unknown || w.Silent) {
			t.Fatalf("gap window must be unknown and non-silent: %+v", w)
		}
	}
}
