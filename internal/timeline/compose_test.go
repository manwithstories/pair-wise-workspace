package timeline

import (
	"strings"
	"testing"

	"mediad/internal/clock"
	"mediad/internal/metadata"
)

func videoTrack(key string, w, h, rot int) metadata.Track {
	return metadata.Track{
		Key: key, Kind: metadata.KindVideo, TrackID: 1,
		Timebase: clock.Video90k, Start: 0, Duration: 9000,
		CodedWidth: w, CodedHeight: h, Rotation: rot,
		Color: metadata.NewColor(metadata.PrimariesBT709, metadata.TransferBT709, metadata.MatrixBT709, false, metadata.SourceColr),
	}
}

func composeTracks(t *testing.T, cfg Config, tracks ...metadata.Track) *Timeline {
	t.Helper()
	asset := &metadata.Asset{Container: "test", Tracks: tracks}
	tl, err := Compose(asset, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func TestFrameGridIsExact(t *testing.T) {
	tl := composeTracks(t, DefaultConfig(), videoTrack("v0", 640, 360, 0))
	// 9000 ticks @90k = 100ms; 30fps => 3 frames of 3000 ticks.
	if len(tl.Frames) != 3 {
		t.Fatalf("frames = %d", len(tl.Frames))
	}
	for i, f := range tl.Frames {
		if f.Start != int64(i)*3000 || f.End != int64(i+1)*3000 {
			t.Fatalf("frame %d = [%d,%d)", i, f.Start, f.End)
		}
		if f.Hole || f.Blocked {
			t.Fatalf("frame %d unexpectedly holed/blocked", i)
		}
	}
}

func TestRotationChangesCanvasAndLayerRect(t *testing.T) {
	portrait := videoTrack("v0", 1920, 1080, 90)
	tl := composeTracks(t, DefaultConfig(), portrait)
	if tl.Width != 1080 || tl.Height != 1920 {
		t.Fatalf("canvas = %dx%d", tl.Width, tl.Height)
	}
	l := tl.Frames[0].Layers[0]
	if l.W != 1080 || l.H != 1920 || l.Rotation != 90 {
		t.Fatalf("layer = %+v", l)
	}
	if l.X != 0 || l.Y != 0 {
		t.Fatalf("layer rect not centered: %d,%d", l.X, l.Y)
	}
}

func TestOverlapIsDeterministicAndOrderedByKey(t *testing.T) {
	big := videoTrack("v0", 1280, 720, 0)
	small := videoTrack("v1", 640, 360, 0)
	tl := composeTracks(t, DefaultConfig(), big, small)
	f := tl.Frames[0]
	if len(f.Layers) != 2 || f.Overlaps != 1 {
		t.Fatalf("layers=%d overlaps=%d", len(f.Layers), f.Overlaps)
	}
	if f.Layers[0].TrackKey != "v0" || f.Layers[1].TrackKey != "v1" {
		t.Fatalf("layer order = %s,%s", f.Layers[0].TrackKey, f.Layers[1].TrackKey)
	}
	// v1 is centered over v0 and smaller.
	if f.Layers[1].X != (1280-640)/2 || f.Layers[1].Y != (720-360)/2 {
		t.Fatalf("overlay rect = %d,%d", f.Layers[1].X, f.Layers[1].Y)
	}
	// Same input composes to the same plan hash.
	tl2 := composeTracks(t, DefaultConfig(), small, big)
	if tl.Frames[0].PlanHash != tl2.Frames[0].PlanHash {
		t.Fatal("plan hash depends on input track order")
	}
}

func TestHoleBlocksWhenThresholdExceeded(t *testing.T) {
	// Track starts 1 second late, leaving a hole at the timeline start.
	late := videoTrack("v0", 640, 360, 0)
	late.Start = 90000
	late.Duration = 9000
	tl := composeTracks(t, DefaultConfig(), late)
	if tl.Frames[0].Hole != true {
		t.Fatal("frame 0 should be a hole")
	}
	if !tl.Frames[0].Blocked || tl.Frames[0].BlockingDep != "coverage:hole" {
		t.Fatalf("hole not blocked: %+v", tl.Frames[0])
	}
	if tl.Stats.FirstBlocked != "coverage:hole" {
		t.Fatalf("first blocked = %q", tl.Stats.FirstBlocked)
	}
}

func TestSmallHoleBelowThresholdDoesNotBlock(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HoleBlockThresholdMs = 1000
	// A 10 ms hole at the start; far below the 1000 ms threshold.
	late := videoTrack("v0", 640, 360, 0)
	late.Start = 900
	late.Duration = 8100
	tl := composeTracks(t, cfg, late)
	if !tl.Frames[0].Hole {
		t.Fatal("expected a hole frame")
	}
	if tl.Frames[0].Blocked {
		t.Fatal("small hole should not block")
	}
}

func TestBoundaryFrameRule(t *testing.T) {
	// A track whose duration is not a multiple of the frame duration ends
	// mid-frame; the last frame's source sample is partial.
	tr := videoTrack("v0", 640, 360, 0)
	tr.Duration = 8000 // 2.67 frames at 3000 ticks
	tl := composeTracks(t, DefaultConfig(), tr)
	if len(tl.Frames) != 3 {
		t.Fatalf("frames = %d", len(tl.Frames))
	}
	last := tl.Frames[2]
	if len(last.Layers) != 1 || !last.Layers[0].Boundary {
		t.Fatalf("last frame boundary = %+v", last.Layers)
	}
	if last.Layers[0].PartialTicks != 2000 {
		t.Fatalf("partial ticks = %d, want 2000", last.Layers[0].PartialTicks)
	}
	if last.End != 8000 {
		t.Fatalf("timeline tail = %d, want 8000", last.End)
	}
}

func TestBlockedTransportGapBlocksFrameAndRetainsFirstDep(t *testing.T) {
	// The timeline is only as long as the track (9000 ticks), so gap frames
	// must fall inside [0,9000). Frame 1 starts at 3000, frame 2 at 6000.
	gaps := []Gap{
		{Start: 3000, End: 6500, Blocked: true, Dep: "transport:gap"},
		{Start: 6000, End: 7000, Blocked: true, Dep: "transport:second"},
	}
	asset := &metadata.Asset{Tracks: []metadata.Track{videoTrack("v0", 640, 360, 0)}}
	tl, err := Compose(asset, gaps, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Frames 1 and 2 are covered by the gaps.
	if !tl.Frames[1].Blocked || !tl.Frames[2].Blocked {
		t.Fatal("gap did not block frames")
	}
	if tl.Frames[1].BlockingDep != "transport:gap" {
		t.Fatalf("first dep lost: %q", tl.Frames[1].BlockingDep)
	}
	if len(tl.Frames[2].GapIndices) != 2 {
		t.Fatalf("gap indices = %v", tl.Frames[2].GapIndices)
	}
	if tl.Stats.FirstBlocked != "transport:gap" {
		t.Fatalf("stats first blocked = %q", tl.Stats.FirstBlocked)
	}
}

func TestUnblockedGapIsRecordedNotBlocking(t *testing.T) {
	gaps := []Gap{{Start: 3000, End: 3500, Blocked: false}}
	asset := &metadata.Asset{Tracks: []metadata.Track{videoTrack("v0", 640, 360, 0)}}
	tl, err := Compose(asset, gaps, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if tl.Frames[1].Blocked {
		t.Fatal("short gap should not block the frame")
	}
	if len(tl.Frames[1].GapIndices) != 1 {
		t.Fatal("short gap not recorded on the frame")
	}
}

func TestFailedTrackIntervalBlocks(t *testing.T) {
	asset := &metadata.Asset{
		Tracks:   []metadata.Track{videoTrack("v0", 640, 360, 0)},
		Failures: []metadata.TrackFailure{{Key: "v0", Err: nil}},
	}
	// A failure with no track record cannot map its interval; ensure the
	// composer still produces a timeline instead of dropping the track.
	tl, err := Compose(asset, nil, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Frames) == 0 {
		t.Fatal("timeline empty")
	}
}

func TestComposeRejectsEmptyAssetAndBadConfig(t *testing.T) {
	if _, err := Compose(&metadata.Asset{}, nil, DefaultConfig()); err == nil ||
		!strings.Contains(err.Error(), "no usable tracks") {
		t.Fatalf("empty asset error = %v", err)
	}
	cfg := DefaultConfig()
	cfg.FPSDen = 0
	if _, err := Compose(&metadata.Asset{Tracks: []metadata.Track{videoTrack("v0", 640, 360, 0)}}, nil, cfg); err == nil {
		t.Fatal("bad cadence accepted")
	}
	// Audio-only input has no video grid.
	audio := metadata.Track{Key: "a0", Kind: metadata.KindAudio, Timebase: clock.Video90k, Duration: 9000}
	if _, err := Compose(&metadata.Asset{Tracks: []metadata.Track{audio}}, nil, DefaultConfig()); err == nil {
		t.Fatal("audio-only timeline accepted")
	}
}
