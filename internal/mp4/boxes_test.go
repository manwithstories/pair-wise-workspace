package mp4

import (
	"os"
	"path/filepath"
	"testing"

	"archive.local/archive/internal/fixture"
	"archive.local/archive/internal/model"
)

func timeline(t *testing.T, trackID uint32, ts uint32, origin int64) *model.Timeline {
	t.Helper()
	tl, err := model.NewTimeline(model.Timebase{TrackID: trackID, Timescale: ts, OriginTicks: origin})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func makeClip(t *testing.T, c fixture.Clip) (*fixture.Layout, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "clip.mp4")
	lay, err := fixture.Generate(p, c)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return lay, p
}

const videoClip = "video"

func TestParseTrackMatchesGeneratedLayout(t *testing.T) {
	lay, p := makeClip(t, fixture.Clip{TrackID: 1, Timescale: 90000, Origin: 324000000,
		Samples: 50, Delta: 3600, KeyEvery: 5, Kind: videoClip, Payload: 200})
	tr, err := ParseFile(p, 1, timeline(t, 1, 90000, 324000000))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tr.Samples) != 50 {
		t.Fatalf("samples = %d", len(tr.Samples))
	}
	for i := range tr.Samples {
		if tr.Samples[i].SourceTicks != lay.SampleTimes[i] {
			t.Fatalf("sample %d source time = %d want %d", i, tr.Samples[i].SourceTicks, lay.SampleTimes[i])
		}
		if tr.Samples[i].Offset != lay.Offsets[i] {
			t.Fatalf("sample %d offset = %d want %d", i, tr.Samples[i].Offset, lay.Offsets[i])
		}
		if tr.Samples[i].Size != lay.Sizes[i] {
			t.Fatalf("sample %d size = %d want %d", i, tr.Samples[i].Size, lay.Sizes[i])
		}
	}
	if len(tr.Sync) != 10 {
		t.Fatalf("sync = %v", tr.Sync)
	}
	if tr.Complete != 50 {
		t.Fatalf("complete = %d", tr.Complete)
	}
	// Unified time keeps the non-zero origin: first sample sits at 1 hour.
	if tr.Samples[0].UnifiedTicks != 3_600_000_000 {
		t.Fatalf("unified = %d", tr.Samples[0].UnifiedTicks)
	}
	if tr.Samples[1].UnifiedTicks-tr.Samples[0].UnifiedTicks != 40000 {
		t.Fatalf("unified delta = %d", tr.Samples[1].UnifiedTicks-tr.Samples[0].UnifiedTicks)
	}
}

func TestTruncatedTailReportsFirstBadSampleAndRecoverableRange(t *testing.T) {
	lay, p := makeClip(t, fixture.Clip{TrackID: 1, Timescale: 90000, Origin: 0,
		Samples: 60, Delta: 3600, KeyEvery: 5, Kind: videoClip, Payload: 400})
	// Drop the last 900 bytes: the tail samples no longer fit in the file.
	if err := fixture.Truncate(p, 900); err != nil {
		t.Fatal(err)
	}
	tr, err := ParseFile(p, 1, timeline(t, 1, 90000, 0))
	if err == nil {
		t.Fatal("expected a truncation defect")
	}
	if model.CodeOf(err) != model.CodeTruncatedSample {
		t.Fatalf("code = %q", model.CodeOf(err))
	}
	me := err.(*model.Error)
	if me.Sample <= 0 || me.Sample >= 60 {
		t.Fatalf("first bad sample = %d", me.Sample)
	}
	// defected sample payload starts beyond the truncation boundary.
	if me.OffsetBytes < 0 {
		t.Fatalf("offset = %d", me.OffsetBytes)
	}
	if tr.Complete != me.Sample {
		t.Fatalf("complete = %d, defect sample = %d", tr.Complete, me.Sample)
	}
	if me.Recoverable == nil || me.Recoverable.LastSample != me.Sample {
		t.Fatalf("recoverable = %+v", me.Recoverable)
	}
	if len(tr.Samples) != 60 {
		t.Fatalf("samples parsed = %d", len(tr.Samples))
	}
	_ = lay
}

func TestCorruptKeyframeTableKeepsSamples(t *testing.T) {
	// Build a valid file, then corrupt stss by pointing at an out-of-range
	// sample number. We locate the stss box and rewrite its first entry.
	_, p := makeClip(t, fixture.Clip{TrackID: 1, Timescale: 90000, Origin: 0,
		Samples: 80, Delta: 3000, KeyEvery: 8, Kind: videoClip, Payload: 120})
	bad, cerr := corruptFirstSync(p, 0xFFFF)
	if cerr != nil {
		t.Fatalf("corrupt: %v", cerr)
	}
	tr, perr := ParseFile(p, 1, timeline(t, 1, 90000, 0))
	if !bad {
		t.Fatal("expected corruption to be applied")
	}
	if perr == nil {
		t.Fatal("expected a keyframe-table defect")
	}
	if model.CodeOf(perr) != model.CodeCorruptKeyTable {
		t.Fatalf("code = %q", model.CodeOf(perr))
	}
	if tr == nil || len(tr.Samples) != 80 {
		t.Fatalf("samples survived = %v", tr)
	}
	if len(tr.Defects) != 1 {
		t.Fatalf("defects = %d", len(tr.Defects))
	}
	// Recovery falls back to the first sample as the only entry point.
	if len(tr.Sync) != 1 || tr.Sync[0] != 1 {
		t.Fatalf("sync fallback = %v", tr.Sync)
	}
}

func TestManifestTrackMissing(t *testing.T) {
	_, p := makeClip(t, fixture.Clip{TrackID: 1, Timescale: 90000, Samples: 10, Delta: 3000, Kind: videoClip, Payload: 40})
	_, err := ParseFile(p, 7, timeline(t, 7, 90000, 0))
	if model.CodeOf(err) != model.CodeManifestReference {
		t.Fatalf("code = %q (%v)", model.CodeOf(err), err)
	}
}

func TestTimescaleMismatchNamesTrack(t *testing.T) {
	_, p := makeClip(t, fixture.Clip{TrackID: 1, Timescale: 90000, Samples: 10, Delta: 3000, Kind: videoClip, Payload: 40})
	_, err := ParseFile(p, 1, timeline(t, 1, 48000, 0))
	if model.CodeOf(err) != model.CodeManifestReference {
		t.Fatalf("code = %q", model.CodeOf(err))
	}
}

func TestAudioTrackAllSync(t *testing.T) {
	_, p := makeClip(t, fixture.Clip{TrackID: 3, Timescale: 48000, Origin: 172800000,
		Samples: 32, Delta: 1024, Kind: "audio", Payload: 90})
	tr, err := ParseFile(p, 3, timeline(t, 3, 48000, 172800000))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tr.Sync) != 32 {
		t.Fatalf("audio sync count = %d", len(tr.Sync))
	}
	if tr.Samples[0].UnifiedTicks != 3_600_000_000 {
		t.Fatalf("audio origin unified = %d", tr.Samples[0].UnifiedTicks)
	}
}

func TestEmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.mp4")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ParseFile(p, 1, timeline(t, 1, 90000, 0))
	if model.CodeOf(err) != model.CodeSourceCorrupt {
		t.Fatalf("code = %q", model.CodeOf(err))
	}
}
