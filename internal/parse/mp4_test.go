package parse

import (
	"path/filepath"
	"testing"

	"avscan/internal/fixture"
	"avscan/internal/model"
)

func writeFixture(t *testing.T, name string, spec fixture.MP4Spec) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".mp4")
	if err := fixture.WriteMP4(path, spec); err != nil {
		t.Fatalf("WriteMP4(%s): %v", name, err)
	}
	return path
}

func healthy() fixture.MP4Spec {
	return fixture.MP4Spec{
		Name: "healthy",
		Tracks: []fixture.TrackSpec{
			{Kind: "video", Timescale: 24_000, SampleCount: 120, SampleDelta: 1000,
				Width: 640, Height: 360, IncludeColr: true, Primaries: 1,
				Transfer: 1, Matrix: 1, IncludeSyncTable: true, KeyFrameEvery: 12},
			{Kind: "audio", Timescale: 48_000, SampleCount: 240, SampleDelta: 1024,
				Channels: 2, SampleRate: 48_000},
			{Kind: "markers", Timescale: 1000, SampleCount: 60, SampleDelta: 500},
		},
	}
}

func TestParseMP4Healthy(t *testing.T) {
	res, err := ParseMP4(writeFixture(t, "healthy", healthy()))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Timeline.Tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(res.Timeline.Tracks))
	}
	wantSamples := map[model.TrackKind]int{
		model.KindVideo: 120, model.KindAudio: 240, model.KindMarkers: 60,
	}
	for _, tr := range res.Timeline.Tracks {
		if want, ok := wantSamples[tr.Kind]; ok && len(tr.Samples) != want {
			t.Errorf("track %d samples = %d, want %d", tr.ID, len(tr.Samples), want)
		}
		if len(tr.ParseFaults) != 0 {
			t.Errorf("track %d unexpected faults: %+v", tr.ID, tr.ParseFaults)
		}
	}
	video := res.Timeline.Tracks[0]
	if len(video.SyncSamples) != 10 { // 120/12
		t.Errorf("sync samples = %d, want 10", len(video.SyncSamples))
	}
	// First video sample starts at zero; second at 1000/24000 s = 41.666...ms.
	if video.Samples[0].StartNS != 0 {
		t.Errorf("first start = %d, want 0", video.Samples[0].StartNS)
	}
	if video.Samples[1].StartNS != 41_666_666 {
		t.Errorf("second start = %d, want 41666666", video.Samples[1].StartNS)
	}
	if !video.Video.HasColr || video.Video.ColourPrimaries != 1 {
		t.Errorf("colr metadata lost: %+v", video.Video)
	}
	// stss marks samples 1,13,25,... (0-based 0,12,24) as sync.
	var syncCount int
	for _, s := range video.Samples {
		if s.IsSync {
			syncCount++
		}
	}
	if syncCount != len(video.SyncSamples) || !video.Samples[0].IsSync || video.Samples[1].IsSync {
		t.Errorf("IsSync flags wrong: count=%d stss=%d", syncCount, len(video.SyncSamples))
	}
	audio := res.Timeline.Tracks[1]
	if audio.Audio.ChannelCount != 2 || audio.Audio.SampleRate != 48_000<<16 {
		t.Errorf("audio metadata lost: %+v", audio.Audio)
	}
}

func TestParseMP4InvalidSampleLength(t *testing.T) {
	spec := healthy()
	spec.Name = "badlen"
	spec.Tracks[1].BadSampleLength = 50 // audio sample #50 claims 1 GiB
	path := writeFixture(t, "badlen", spec)
	res, err := ParseMP4(path)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tr := range res.Timeline.Tracks {
		for _, f := range tr.ParseFaults {
			if f.Kind == model.FaultOffsetOutOfRange && f.HasByte {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected out-of-range fault with byte offset for bad length")
	}
}

func TestParseMP4Truncated(t *testing.T) {
	spec := healthy()
	spec.Name = "trunc"
	spec.TruncateAt = 600 // cuts through mdat, before moov
	path := writeFixture(t, "trunc", spec)
	res, err := ParseMP4(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GlobalFaults) == 0 {
		t.Error("truncated file must produce a global structural fault")
	}
	if res.GlobalFaults[0].ByteOffset <= 0 {
		t.Error("truncation fault must carry a byte offset")
	}
}

func TestParseMP4BadEditList(t *testing.T) {
	spec := healthy()
	spec.Name = "badedits"
	spec.Tracks[0].BadClipEdit = true
	path := writeFixture(t, "badedits", spec)
	res, err := ParseMP4(path)
	if err != nil {
		t.Fatal(err)
	}
	edits := res.Timeline.Tracks[0].Edits
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
	trackDur := int64(spec.Tracks[0].SampleCount) * int64(spec.Tracks[0].SampleDelta)
	if edits[0].MediaTime <= trackDur {
		t.Error("out-of-bounds edit media time was not preserved")
	}
}
