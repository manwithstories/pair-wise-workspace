package audio

import (
	"testing"

	"fieldqc/internal/clock"
	"fieldqc/internal/fault"
)

func TestDownmix51CoefficientsAndLength(t *testing.T) {
	// One frame FL FR FC LFE SL SR; LFE is dropped, -3 dB on FC/SL/SR.
	in := []int16{1000, 2000, 1000, 999, 1000, 0}
	out := Downmix51(in)
	if len(out) != 2 {
		t.Fatalf("len(out)=%d, want 2", len(out))
	}
	if out[0] != 2414 { // 1000 + 1000*.707 + 1000*.707
		t.Fatalf("left=%d, want 2414", out[0])
	}
	if out[1] != 2707 { // 2000 + 1000*.707 + 0
		t.Fatalf("right=%d, want 2707", out[1])
	}
}

func TestNormalizeRejectsUnsupportedMapping(t *testing.T) {
	p := &PCM{SampleRate: 48000, Channels: 3, Samples: make([]int16, 30)}
	if _, err := Normalize(p, 48000, LayoutStereo); err == nil {
		t.Fatal("3-channel -> stereo accepted, want unsupported error")
	}
}

func TestNormalize51_44100ToStereo48000(t *testing.T) {
	frames := 44100 // exactly 1 s at 44.1 kHz
	p := &PCM{SampleRate: 44100, Channels: Layout51, Samples: make([]int16, frames*6)}
	out, err := Normalize(p, 48000, LayoutStereo)
	if err != nil {
		t.Fatal(err)
	}
	if out.Channels != LayoutStereo {
		t.Fatalf("channels=%d, want 2", out.Channels)
	}
	if out.SampleRate != 48000 {
		t.Fatalf("rate=%d, want 48000", out.SampleRate)
	}
	if got := out.Frames(); got != 48000 { // exact rational ratio for 1 s
		t.Fatalf("frames=%d, want 48000", got)
	}
	if len(out.Samples) != 2*48000 {
		t.Fatalf("samples=%d, want %d", len(out.Samples), 2*48000)
	}
}

func TestCutOnBlockBoundaryHasNoEdge(t *testing.T) {
	p := &PCM{SampleRate: 48000, Channels: 2, Samples: make([]int16, 4096*2)}
	before, after, e := Cut(p, 2048, 1024)
	if e.Pad != 0 || e.Gap != 0 {
		t.Fatalf("boundary cut edge=%+v, want zero pad/gap", e)
	}
	if before.Frames() != 2048 || after.Frames() != 2048 {
		t.Fatalf("split frames=(%d,%d), want (2048,2048)", before.Frames(), after.Frames())
	}
	if _, ok := EdgeSpan("tr", 48000, e); ok {
		t.Fatal("EdgeSpan ok=true for block-aligned cut, want false")
	}
}

func TestCutInsideBlockRecordsPadding(t *testing.T) {
	p := &PCM{SampleRate: 48000, Channels: 2, Samples: make([]int16, 4096*2)}
	_, _, e := Cut(p, 2048+13, 1024) // 13 frames into a block
	if e.Pad != 1024-13 {
		t.Fatalf("pad=%d, want %d", e.Pad, 1011)
	}
	if e.Gap != 0 {
		t.Fatalf("gap=%d, want 0", e.Gap)
	}
	sp, ok := EdgeSpan("card0/a", 48000, e)
	if !ok {
		t.Fatal("EdgeSpan ok=false for off-block cut, want true")
	}
	if sp.Code != fault.CodeAudioEdge || sp.Track != "card0/a" {
		t.Fatalf("span=%+v, want AUDIO_EDGE on card0/a", sp)
	}
	if want, _ := clock.New(2061, 48000); sp.Start.Cmp(want) != 0 {
		t.Fatalf("start=%s, want %s", sp.Start, want)
	}
}

func TestCutBeyondMediaRecordsGap(t *testing.T) {
	p := &PCM{SampleRate: 48000, Channels: 2, Samples: make([]int16, 100*2)}
	before, after, e := Cut(p, 500, 1024)
	if e.Gap != 400 { // requested 500, only 100 present
		t.Fatalf("gap=%d, want 400", e.Gap)
	}
	if before.Frames() != 100 || after.Frames() != 0 {
		t.Fatalf("frames=(%d,%d), want (100,0)", before.Frames(), after.Frames())
	}
	if _, ok := EdgeSpan("tr", 48000, e); !ok {
		t.Fatal("EdgeSpan ok=false for gapped cut, want true")
	}
}
