package pipeline

import (
	"testing"

	"mediarun/internal/ingest"
	"mediarun/internal/mediaerr"
	"mediarun/internal/timebase"
)

func TestEnforceAlignmentRejectsDrift(t *testing.T) {
	info := ingest.ContainerInfo{
		Tracks: []ingest.TrackDesc{
			{ID: "video/0", Kind: ingest.KindVideo, Timescale: 30000},
			{ID: "audio/1", Kind: ingest.KindAudio, Timescale: 48000},
		},
	}
	// Video: 1 second. Audio: only half a second -> 500 ms drift.
	stats := &Stats{
		ticks: map[string]int64{"video/0": 30000, "audio/1": 24000},
		bytes: map[string]int64{"video/0": 1000, "audio/1": 1000},
		scale: map[string]int64{"video/0": 30000, "audio/1": 48000},
	}
	err := enforceAlignment(Options{}, stats, info)
	if !mediaerr.Is(err, mediaerr.CodeManifestMisalign) {
		t.Fatalf("want MANIFEST_MISALIGN, got %v", err)
	}
	if stats.ManifestAlignMillis != 500 {
		t.Fatalf("alignment = %d ms, want 500", stats.ManifestAlignMillis)
	}
}

func TestEnforceAlignmentAcceptsTightDrift(t *testing.T) {
	info := ingest.ContainerInfo{
		Tracks: []ingest.TrackDesc{
			{ID: "video/0", Kind: ingest.KindVideo, Timescale: 30000},
			{ID: "audio/1", Kind: ingest.KindAudio, Timescale: 48000},
		},
	}
	// Identical one-second timelines: zero drift must pass.
	stats := &Stats{
		ticks: map[string]int64{"video/0": 30000, "audio/1": 48000},
		bytes: map[string]int64{"video/0": 1000, "audio/1": 1000},
		scale: map[string]int64{"video/0": 30000, "audio/1": 48000},
	}
	if err := enforceAlignment(Options{}, stats, info); err != nil {
		t.Fatalf("aligned tracks must pass: %v", err)
	}
	if stats.ManifestAlignMillis != 0 {
		t.Fatalf("alignment = %d, want 0", stats.ManifestAlignMillis)
	}
}

func TestExactRationalAlignment(t *testing.T) {
	// 29.97 fps frame window must stay exactly representable (no float drift).
	tb := timebase.NewTimebase(30000)
	oneFrame := tb.ToSeconds(1001)
	twoFrames := oneFrame.Add(oneFrame)
	want := timebase.New(2002, 30000)
	if !twoFrames.Equal(want) {
		t.Fatalf("two frames = %v, want %v", twoFrames, want)
	}
}
