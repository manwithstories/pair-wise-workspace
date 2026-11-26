package hls

import (
	"strings"
	"testing"

	"deliver/internal/cue"
	"deliver/internal/timecode"
)

func TestBoundariesFrameQuantized(t *testing.T) {
	bounds := Boundaries(180_000, 2000, timecode.FPS25)
	if len(bounds) != 90 {
		t.Fatalf("got %d segments, want 90", len(bounds))
	}
	for i, b := range bounds {
		if b != int64(i)*2000 {
			t.Fatalf("boundary %d = %dms", i, b)
		}
	}
	// 29.97: boundaries must still land on whole frames.
	for _, b := range Boundaries(180_000, 2000, timecode.FPS2997) {
		fr := timecode.MsToFrames(b, timecode.FPS2997)
		if got := timecode.FramesToMs(fr, timecode.FPS2997); got != b {
			t.Fatalf("boundary %dms not on a frame (%d)", b, got)
		}
	}
}

func TestCueClippedNotDuplicated(t *testing.T) {
	cues := []cue.Cue{
		{Index: 0, StartMs: 1500, EndMs: 3500, Text: "crosses"},
		{Index: 1, StartMs: 2100, EndMs: 2600, Text: "inside"},
	}
	segs := Segments(6000, 2000, timecode.FPS25, cues)
	if len(segs[0].Cues) != 1 || segs[0].Cues[0].EndMs != 2000 {
		t.Fatalf("crossing cue not clipped: %+v", segs[0].Cues)
	}
	total := 0
	for _, s := range segs {
		total += len(s.Cues)
	}
	if total != 2 {
		t.Fatalf("cues duplicated or lost: %d", total)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	segs := Segments(180_000, 2000, timecode.FPS25, nil)
	rends := []Rendition{{Name: "v0", Bandwidth: 800_000},
		{Name: "v1", Bandwidth: 2_500_000}, {Name: "v2", Bandwidth: 6_000_000}}
	master := BuildMaster(rends)
	for _, r := range rends {
		if !strings.Contains(master, r.Name+"/index.m3u8") {
			t.Fatalf("master missing %s", r.Name)
		}
		pl, err := BuildMediaPlaylist(r, segs)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyMediaPlaylist(pl, segs, 40.0); err != nil {
			t.Fatalf("verify %s: %v", r.Name, err)
		}
	}
}

func TestReassembler(t *testing.T) {
	r := NewReassembler(0, 4)
	r.Push(Event{Seq: 1}) // out of order
	r.Push(Event{Seq: 1}) // duplicate
	if out := r.Push(Event{Seq: 0}); len(out) != 2 {
		t.Fatalf("reorder failed: %d events", len(out))
	}
	r.Cache(Event{Seq: 2}) // retransmissions heal the gap
	r.Cache(Event{Seq: 3})
	out := r.Push(Event{Seq: 4})
	if len(out) != 3 || out[0].Seq != 2 {
		t.Fatalf("retry cache heal failed: %+v", out)
	}
	if r.Dups != 1 || r.Healed != 2 || r.Lost != 0 {
		t.Fatalf("counters: dups=%d healed=%d lost=%d", r.Dups, r.Healed, r.Lost)
	}
	// Unhealed gap outlives the window and is counted lost.
	r2 := NewReassembler(0, 2)
	r2.Push(Event{Seq: 1})
	r2.Push(Event{Seq: 2})
	r2.Push(Event{Seq: 3})
	if r2.Lost != 1 {
		t.Fatalf("lost = %d, want 1", r2.Lost)
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	p := t.TempDir() + "/ckpt/state"
	if _, ok, _ := ReadCheckpoint(p); ok {
		t.Fatal("no checkpoint expected")
	}
	if err := WriteCheckpoint(p, 41); err != nil {
		t.Fatal(err)
	}
	if err := WriteCheckpoint(p, 42); err != nil {
		t.Fatal(err)
	}
	seq, ok, err := ReadCheckpoint(p)
	if err != nil || !ok || seq != 42 {
		t.Fatalf("got %d,%v,%v", seq, ok, err)
	}
}
