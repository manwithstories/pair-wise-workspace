package render

import (
	"strings"
	"testing"

	"avcheck/internal/clock"
	"avcheck/internal/timeline"
)

func makeMix(frames, ch int) *timeline.Mixed {
	tpf := int(clock.TicksPerFrame())
	nFrames := (frames / tpf) * tpf
	m := &timeline.Mixed{
		Channels:       ch,
		StartTick:      0,
		Frames:         nFrames,
		Samples:        make([]int32, nFrames*ch),
		PerTrackFrames: map[uint32]int{1: nFrames, 2: nFrames},
	}
	for i := range m.Samples {
		m.Samples[i] = int32((i*7)%997 - 498)
	}
	return m
}

func TestFrameCountAndTimestamps(t *testing.T) {
	m := makeMix(int(clock.TicksPerFrame())*3, 2)
	r := NewRenderer(clock.SampleRate)
	seq := r.Render(m)
	if len(seq.Frames) != 3 {
		t.Fatalf("frames=%d, want 3", len(seq.Frames))
	}
	if seq.Frames[1].Tick != clock.TicksPerFrame() {
		t.Fatalf("second frame tick=%d", seq.Frames[1].Tick)
	}
	if seq.Frames[1].MP4Timestamp != uint64(clock.TicksPerFrame()) {
		t.Fatalf("mp4 ts=%d", seq.Frames[1].MP4Timestamp)
	}
	if seq.Frames[0].SampleCount != int(clock.TicksPerFrame()) {
		t.Fatalf("sample count=%d", seq.Frames[0].SampleCount)
	}
}

func TestDeterministicHashAcrossRuns(t *testing.T) {
	r := NewRenderer(clock.SampleRate)
	m1 := makeMix(int(clock.TicksPerFrame())*4, 2)
	m2 := makeMix(int(clock.TicksPerFrame())*4, 2)
	s1 := r.Render(m1)
	s2 := r.Render(m2)
	if s1.FrameHash != s2.FrameHash {
		t.Fatalf("chained hash %016x != %016x for identical input", s1.FrameHash, s2.FrameHash)
	}
	for i := range s1.Frames {
		if s1.Frames[i].Hash != s2.Frames[i].Hash {
			t.Fatalf("frame %d hash differs", i)
		}
	}
}

func TestDifferentInputProducesDifferentHash(t *testing.T) {
	r := NewRenderer(clock.SampleRate)
	m1 := makeMix(int(clock.TicksPerFrame())*2, 1)
	m2 := makeMix(int(clock.TicksPerFrame())*2, 1)
	m2.Samples[10]++ // perturb a single sample
	if r.Render(m1).FrameHash == r.Render(m2).FrameHash {
		t.Fatal("perturbed input produced identical frame hash")
	}
}

func TestSummaryIsStableAndComplete(t *testing.T) {
	r := NewRenderer(clock.SampleRate)
	seq := r.Render(makeMix(int(clock.TicksPerFrame())*2, 2))
	a, b := seq.Summary(), seq.Summary()
	if a != b {
		t.Fatal("summary not deterministic")
	}
	for _, want := range []string{"frames=2", "frame_hash=0x", "track1_frames=", "track2_frames=",
		"mp4_first=", "mp4_last="} {
		if !strings.Contains(a, want) {
			t.Fatalf("summary %q missing %q", a, want)
		}
	}
}

func TestMP4TimescaleScaling(t *testing.T) {
	// Half the sample-rate timescale -> timestamps halve.
	r := NewRenderer(clock.SampleRate / 2)
	seq := r.Render(makeMix(int(clock.TicksPerFrame())*2, 1))
	want := uint64(clock.TicksPerFrame()) / 2
	if seq.Frames[1].MP4Timestamp != want {
		t.Fatalf("scaled mp4 ts=%d, want %d", seq.Frames[1].MP4Timestamp, want)
	}
}
