package timeline

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"avpipe/internal/media"
)

func mustTimeline(t *testing.T) *Timeline {
	t.Helper()
	tl, err := New(media.Rate{Num: 48000, Den: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := tl.Add(Track{
		ID: "mix", Kind: Audio, Rate: media.Rate{Num: 48000, Den: 1},
		Channels: 2, Labels: []string{"L", "R"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tl.Add(Track{
		ID: "cam", Kind: Video,
		Anchors: []Anchor{
			{At: 0, Label: "k0"},
			{At: 20 * time.Millisecond, Label: "k1"},
			{At: 60 * time.Millisecond, Label: "k2"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tl.Finalize(); err != nil {
		t.Fatal(err)
	}
	return tl
}

func TestKeyframesUseSharedClock(t *testing.T) {
	tl := mustTimeline(t)
	kf := tl.Keyframes()
	want := []int64{0, 960, 2880}
	if len(kf) != len(want) {
		t.Fatalf("keyframes=%v", kf)
	}
	for i := range want {
		if kf[i] != want[i] {
			t.Fatalf("keyframes=%v want %v", kf, want)
		}
	}
	if n := tl.AnchorsIn(0, 960); n != 1 {
		t.Fatalf("anchors in [0,960)=%d", n)
	}
	if n := tl.AnchorsIn(1, 2881); n != 2 {
		t.Fatalf("anchors in [1,2881)=%d", n)
	}
	if tl.Stats().Anchors != 3 {
		t.Fatalf("stats=%+v", tl.Stats())
	}
}

func TestDuplicateAndConflict(t *testing.T) {
	tl := mustTimeline(t)
	dup, err := tl.Accept("mix", 5, 0xabc)
	if err != nil || dup {
		t.Fatalf("first accept dup=%v err=%v", dup, err)
	}
	dup, err = tl.Accept("mix", 5, 0xabc)
	if err != nil || !dup {
		t.Fatalf("repeat should be dropped: dup=%v err=%v", dup, err)
	}
	_, err = tl.Accept("mix", 5, 0xdef)
	if err == nil {
		t.Fatal("conflicting hash accepted")
	}
	if !errors.Is(err, media.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if media.ExitFor(err) != media.ExitInput {
		t.Fatalf("exit=%d", media.ExitFor(err))
	}
	if tl.Stats().Duplicates != 1 {
		t.Fatalf("stats=%+v", tl.Stats())
	}
}

func TestSequenceRewindRejected(t *testing.T) {
	tl := mustTimeline(t)
	if _, err := tl.Accept("mix", 10, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Accept("mix", 3, 2); err == nil {
		t.Fatal("rewind accepted")
	}
}

func TestAdvanceGapAndReorder(t *testing.T) {
	tl := mustTimeline(t)
	gap, err := tl.Advance("mix", 0, 480)
	if err != nil || gap != 0 {
		t.Fatalf("gap=%d err=%v", gap, err)
	}
	gap, err = tl.Advance("mix", 480, 480)
	if err != nil || gap != 0 {
		t.Fatalf("gap=%d err=%v", gap, err)
	}
	gap, err = tl.Advance("mix", 1200, 480)
	if err != nil || gap != 240 {
		t.Fatalf("gap=%d err=%v", gap, err)
	}
	if tl.Stats().MissingFrames != 240 {
		t.Fatalf("stats=%+v", tl.Stats())
	}
	if _, err := tl.Advance("mix", 1600, 480); err == nil {
		t.Fatal("overlap accepted")
	}
}

func TestTrackValidation(t *testing.T) {
	tl, _ := New(media.Rate{Num: 48000, Den: 1})
	cases := map[string]Track{
		"no labels":      {ID: "a", Kind: Audio, Rate: media.Rate{Num: 48000, Den: 1}, Channels: 2},
		"bad rate":       {ID: "b", Kind: Audio, Rate: media.Rate{}, Channels: 1, Labels: []string{"M"}},
		"empty label":    {ID: "c", Kind: Audio, Rate: media.Rate{Num: 48000, Den: 1}, Channels: 1, Labels: []string{""}},
		"video samples":  {ID: "d", Kind: Video, Channels: 2, Anchors: []Anchor{{At: 0}}},
		"video no ancho": {ID: "e", Kind: Video},
		"kind":           {ID: "f", Kind: "subtitle"},
		"empty id":       {Kind: Audio, Rate: media.Rate{Num: 48000, Den: 1}, Channels: 1, Labels: []string{"M"}},
	}
	for name, tr := range cases {
		if err := tl.Add(tr); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if media.ExitFor(tl.Add(tr)) != media.ExitUsage {
			t.Fatalf("%s: exit", name)
		}
	}
	if err := tl.Add(Track{ID: "v", Kind: Video, Anchors: []Anchor{{At: 10 * time.Millisecond}, {At: time.Millisecond}}}); err == nil {
		t.Fatal("unordered anchors accepted")
	}
}

func TestTrackLimitEnforced(t *testing.T) {
	tl, _ := New(media.Rate{Num: 48000, Den: 1})
	for i := 0; i < MaxTracks; i++ {
		err := tl.Add(Track{ID: fmt.Sprintf("v%d", i), Kind: Video, Anchors: []Anchor{{At: 0}}})
		if err != nil {
			t.Fatalf("track %d: %v", i, err)
		}
	}
	err := tl.Add(Track{ID: "overflow", Kind: Video, Anchors: []Anchor{{At: 0}}})
	if err == nil {
		t.Fatal("track limit not enforced")
	}
	if media.ExitFor(err) != media.ExitUsage {
		t.Fatalf("exit=%d", media.ExitFor(err))
	}
}

func TestFinalizeRequiresAudio(t *testing.T) {
	tl, _ := New(media.Rate{Num: 48000, Den: 1})
	if err := tl.Finalize(); err == nil {
		t.Fatal("empty timeline finalized")
	}
	if err := tl.Add(Track{ID: "v", Kind: Video, Anchors: []Anchor{{At: 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := tl.Finalize(); err == nil {
		t.Fatal("video-only timeline finalized")
	}
}
