package media

import (
	"strings"
	"testing"

	"medrelay/internal/timeline"
)

func vframes(n int) []Frame {
	var out []Frame
	for i := 0; i < n; i++ {
		out = append(out, Frame{Kind: 'v', PTS: int64(i) * 3000, Dur: 3000, TS: timeline.Video, Data: []byte{byte(i)}})
	}
	return out
}

func TestSplitAligned(t *testing.T) {
	// 30fps video: frame every 100/3 ms; cut at 1s = frame 30 exactly.
	frames := vframes(90)
	segs, err := Split(frames, []int64{1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if len(segs[0].Frames) != 30 || len(segs[1].Frames) != 60 {
		t.Fatalf("split sizes %d/%d", len(segs[0].Frames), len(segs[1].Frames))
	}
	if segs[0].StartMs != 0 || segs[0].EndMs != 1000 {
		t.Fatalf("seg0 %+v", segs[0])
	}
}

func TestSplitUnrepresentable(t *testing.T) {
	frames := vframes(10) // frames at 0, 33, 66, ... ms
	_, err := Split(frames, []int64{50})
	te, ok := err.(*timeline.Error)
	if !ok || te.Code != timeline.ErrBoundary {
		t.Fatalf("want unrepresentable_boundary, got %v", err)
	}
	if te.StartMs >= 50 || te.EndMs <= 50 {
		t.Fatalf("error interval should straddle the cut: %v", te)
	}
}

func TestDetectSilenceAllChannels(t *testing.T) {
	rate := 48000
	mk := func() []int16 { return make([]int16, rate) } // 1s per channel
	a, b := mk(), mk()
	// Loud 100ms in the middle of both channels.
	for i := rate / 2; i < rate/2+rate/10; i++ {
		a[i], b[i] = 5000, 5000
	}
	got := DetectSilence([][]int16{a, b}, rate, 100, 200)
	if len(got) != 2 {
		t.Fatalf("got %+v, want 2 silence intervals", got)
	}
	if got[0].StartMs != 0 || got[1].EndMs != 1000 {
		t.Fatalf("bad intervals %+v", got)
	}
}

func TestDetectSilenceOneChannelLoud(t *testing.T) {
	rate := 48000
	a := make([]int16, rate)
	b := make([]int16, rate)
	for i := range b {
		b[i] = 3000 // right channel never silent
	}
	if got := DetectSilence([][]int16{a, b}, rate, 100, 100); len(got) != 0 {
		t.Fatalf("one loud channel must veto silence, got %+v", got)
	}
}

func TestDetectSilenceBelowMinDuration(t *testing.T) {
	rate := 48000
	a := make([]int16, rate/10) // 100ms of silence, gate is 200ms
	if got := DetectSilence([][]int16{a}, rate, 100, 200); len(got) != 0 {
		t.Fatalf("short silence must not trigger, got %+v", got)
	}
}

func TestDeduperDuplicate(t *testing.T) {
	d := NewDeduper(500)
	frames := []Frame{
		{Kind: 'a', PTS: 0, Dur: 24000, TS: timeline.Audio, Data: []byte("hello")},
		{Kind: 'a', PTS: 24000, Dur: 24000, TS: timeline.Audio, Data: []byte("world")},
	}
	rep1 := d.Process("seg-0.m4s", frames, 0, 1000)
	if len(rep1.Dups) != 0 {
		t.Fatalf("first occurrence must not be a dup: %+v", rep1.Dups)
	}
	rep2 := d.Process("seg-1.m4s", frames, 0, 1000)
	if len(rep2.Dups) != 2 {
		t.Fatalf("want 2 dups, got %+v", rep2.Dups)
	}
	if rep2.Dups[0].DupOf.Source != "seg-0.m4s" {
		t.Fatalf("dup mapping lost: %+v", rep2.Dups[0])
	}
}

func TestDeduperBoundaryWindow(t *testing.T) {
	d := NewDeduper(1000)
	frames := []Frame{{Kind: 'a', PTS: 0, Dur: 24000, TS: timeline.Audio, Data: []byte("x")}}
	rep := d.Process("seg-9.m4s", frames, 0, 500) // tail shorter than window
	if len(rep.Reasons) != 1 || !strings.Contains(rep.Reasons[0], "boundary window") {
		t.Fatalf("want boundary-window reason, got %+v", rep.Reasons)
	}
	if len(rep.Kept) != 1 {
		t.Fatalf("original must be kept: %+v", rep.Kept)
	}
}
