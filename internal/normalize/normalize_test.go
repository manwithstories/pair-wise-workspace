package normalize

import (
	"testing"

	"medrelay/internal/timeline"
)

func TestNormalizeAudioPassthrough(t *testing.T) {
	n, err := NormalizeAudio(AudioMeta{SampleRate: 48000, Channels: 2, Layout: "stereo"})
	if err != nil {
		t.Fatal(err)
	}
	if n.ResampleNum != 1 || n.ResampleDen != 1 {
		t.Fatalf("48k->48k should be 1:1, got %d:%d", n.ResampleNum, n.ResampleDen)
	}
}

func TestNormalizeAudioResample(t *testing.T) {
	n, err := NormalizeAudio(AudioMeta{SampleRate: 44100, Channels: 2, Layout: "stereo"})
	if err != nil {
		t.Fatal(err)
	}
	// 48000/44100 = 160/147.
	if n.ResampleNum != 160 || n.ResampleDen != 147 {
		t.Fatalf("got %d:%d, want 160:147", n.ResampleNum, n.ResampleDen)
	}
}

func TestNormalizeAudioRejectsBadLayout(t *testing.T) {
	if _, err := NormalizeAudio(AudioMeta{SampleRate: 48000, Channels: 3, Layout: "stereo"}); err == nil {
		t.Fatal("channel/layout mismatch must be rejected")
	}
	if _, err := NormalizeAudio(AudioMeta{SampleRate: 48000, Channels: 2, Layout: "quad"}); err == nil {
		t.Fatal("unknown layout must be rejected")
	}
	if _, err := NormalizeAudio(AudioMeta{SampleRate: 4000, Channels: 1, Layout: "mono"}); err == nil {
		t.Fatal("out-of-range rate must be rejected")
	}
}

func TestColorConflictQuarantines(t *testing.T) {
	base := ColorMeta{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	next := ColorMeta{Primaries: "bt2020", Transfer: "pq", Matrix: "bt2020nc"}
	q, err := ReconcileColor(base, next, "seg-4.m4s", 24000, 30000)
	if !q || err == nil {
		t.Fatalf("conflict must quarantine, got q=%v err=%v", q, err)
	}
	te := err.(*timeline.Error)
	if te.Segment != "seg-4.m4s" || te.StartMs != 24000 || te.EndMs != 30000 {
		t.Fatalf("error not located: %v", te)
	}
	// Baseline unchanged by the conflict.
	if base.Primaries != "bt709" {
		t.Fatal("baseline must not be overwritten")
	}
}

func TestColorEqualPasses(t *testing.T) {
	c := ColorMeta{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	q, err := ReconcileColor(c, c, "seg-0.m4s", 0, 6000)
	if q || err != nil {
		t.Fatalf("equal metadata must pass: q=%v err=%v", q, err)
	}
}

func TestColorUnknownRejected(t *testing.T) {
	bad := ColorMeta{Primaries: "aces", Transfer: "bt709", Matrix: "bt709"}
	if err := ValidateColor(bad); err == nil {
		t.Fatal("unknown primaries must be rejected")
	}
}
