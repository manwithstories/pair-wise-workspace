package segment

import (
	"testing"

	"medpipe/internal/clock"
	"medpipe/internal/mp4"
)

func makeVideoTrack() *mp4.Track {
	// 30 fps, timescale 30000, 3 seconds; keyframes every 10 frames.
	t := &mp4.Track{
		ID: 1, Kind: mp4.KindVideo, Codec: "avc1", Timescale: 30000,
		Duration: clock.MustNew(90000, 30000),
	}
	for i := 0; i < 90; i++ {
		t.Samples = append(t.Samples, mp4.SamplePoint{
			DTS:        clock.MustNew(uint64(i)*1000, 30000),
			ByteOffset: int64(1000 + i*100),
			Size:       90,
			IsSync:     i%10 == 0,
		})
	}
	return t
}

func makeAudioTrack() *mp4.Track {
	// 48 kHz, AAC packets of 1024 samples (~21.33 ms), 3 seconds.
	t := &mp4.Track{
		ID: 2, Kind: mp4.KindAudio, Codec: "mp4a", Timescale: 48000,
		Channels: 2, SampleRate: 48000,
		Duration: clock.MustNew(144000, 48000),
	}
	for i := 0; i < 141; i++ {
		t.Samples = append(t.Samples, mp4.SamplePoint{
			DTS:        clock.MustNew(uint64(i)*1024, 48000),
			ByteOffset: int64(5000 + i*200),
			Size:       190,
		})
	}
	return t
}

func makeContainer() *mp4.Container {
	return &mp4.Container{
		Timescale: 1000,
		Duration:  clock.MustNew(3000, 1000),
		Tracks:    []*mp4.Track{makeVideoTrack(), makeAudioTrack()},
	}
}

func TestPlanAlignsToKeyframeAndPacket(t *testing.T) {
	p, err := NewPlanner(makeContainer())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.Build(1000) // one segment per second
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Boundaries) != 3 {
		t.Fatalf("boundaries = %d, want 3", len(plan.Boundaries))
	}
	for i, b := range plan.Boundaries {
		if b.Index != i {
			t.Fatalf("index wrong: %+v", b)
		}
		// Every chosen video sample must be a sync sample.
		if !p.video.Samples[videoSampleIndex(p, b)].IsSync {
			t.Fatalf("boundary %d not on a keyframe: %+v", i, b)
		}
		// Aligned boundaries never start after the requested window start.
		if b.VideoTick.Millis() > uint64(b.StartMillis) {
			t.Fatalf("video tick %d past window start %d", b.VideoTick.Millis(), b.StartMillis)
		}
		if b.AudioTick.Millis() > uint64(b.StartMillis) {
			t.Fatalf("audio tick %d past window start %d", b.AudioTick.Millis(), b.StartMillis)
		}
		if b.VideoByte <= 0 || b.AudioByte <= 0 {
			t.Fatalf("missing byte offsets: %+v", b)
		}
	}
}

func videoSampleIndex(p *Planner, b Boundary) int {
	for i, s := range p.video.Samples {
		if s.ByteOffset == b.VideoByte {
			return i
		}
	}
	return -1
}

func TestPlanSnapsBeforeNotAfter(t *testing.T) {
	p, _ := NewPlanner(makeContainer())
	plan, err := p.Build(500) // 500 ms windows
	if err != nil {
		t.Fatal(err)
	}
	// The first window starts at 0; its keyframe must be frame 0 (0 ms).
	if plan.Boundaries[0].VideoTick.Millis() != 0 {
		t.Fatalf("first boundary video tick = %d, want 0",
			plan.Boundaries[0].VideoTick.Millis())
	}
	// Window at 500 ms snaps to the keyframe at 333 ms (frame 10), not 666.
	b := plan.Boundaries[1]
	if b.VideoTick.Millis() != 333 {
		t.Fatalf("500 ms window snapped to %d ms, want 333", b.VideoTick.Millis())
	}
}

func TestPlanAudioOnly(t *testing.T) {
	c := &mp4.Container{
		Timescale: 1000, Duration: clock.MustNew(3000, 1000),
		Tracks: []*mp4.Track{makeAudioTrack()},
	}
	p, err := NewPlanner(c)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.Build(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Boundaries) != 3 || plan.Boundaries[0].VideoByte != 0 {
		t.Fatalf("audio-only plan wrong: %+v", plan.Boundaries)
	}
}

func TestPlanRejectsNoAudio(t *testing.T) {
	c := &mp4.Container{
		Timescale: 1000, Duration: clock.MustNew(1000, 1000),
		Tracks: []*mp4.Track{makeVideoTrack()},
	}
	if _, err := NewPlanner(c); err == nil {
		t.Fatal("container without audio accepted")
	}
}

func TestPlanInvalidDuration(t *testing.T) {
	p, _ := NewPlanner(makeContainer())
	if _, err := p.Build(0); err == nil {
		t.Fatal("zero target duration accepted")
	}
}

func TestColorPolicyPreserveAndConflict(t *testing.T) {
	src := &mp4.ColorMeta{ColorType: "nclx", ColourPrimaries: 1,
		TransferCharacteristics: 1, MatrixCoefficients: 1}
	// No override: source preserved.
	got, err := ColorPolicy{}.Resolve(src)
	if err != nil || got != src {
		t.Fatalf("source not preserved: %v %v", got, err)
	}
	// Matching override is fine.
	match := *src
	got, err = ColorPolicy{Override: &match}.Resolve(src)
	if err != nil || got == nil {
		t.Fatalf("matching override rejected: %v", err)
	}
	// Conflicting override must error, not silently overwrite.
	conflict := *src
	conflict.ColourPrimaries = 9
	_, err = ColorPolicy{Override: &conflict}.Resolve(src)
	if !clock.IsKind(err, clock.KindInconsistent) {
		t.Fatalf("color conflict not flagged: %v", err)
	}
}

func TestColorFFmpegArgs(t *testing.T) {
	c := &mp4.ColorMeta{ColorType: "nclx", ColourPrimaries: 1,
		TransferCharacteristics: 1, MatrixCoefficients: 1}
	args := ffmpegColorArgs(c)
	if len(args) == 0 {
		t.Fatal("nclx metadata produced no ffmpeg args")
	}
	if ffmpegColorArgs(nil) != nil {
		t.Fatal("nil color should produce no args")
	}
}

func TestSegmentNameAndSecs(t *testing.T) {
	if segmentName(0) != "seg-000.mp4" || segmentName(7) != "seg-007.mp4" ||
		segmentName(42) != "seg-042.mp4" {
		t.Fatalf("segment naming wrong: %s %s %s",
			segmentName(0), segmentName(7), segmentName(42))
	}
	if secs(1500) != "1.500" || secs(333) != "0.333" || secs(0) != "0.000" {
		t.Fatalf("secs formatting wrong: %s %s %s", secs(1500), secs(333), secs(0))
	}
}
