package timeline

import (
	"testing"

	"avscan/internal/model"
)

func buildTimeline() *model.Timeline {
	video := &model.Track{ID: 1, Kind: model.KindVideo, Timescale: 24_000}
	for i := 0; i < 12; i++ {
		video.Samples = append(video.Samples, model.SampleEvent{
			TrackID: 1, Index: uint32(i),
			StartNS: int64(i) * 41_666_666, DurationNS: 41_666_666,
			Offset: uint64(i * 50), Size: 50,
		})
	}
	a1 := &model.Track{ID: 2, Kind: model.KindAudio, Timescale: 48_000,
		Audio: model.AudioMeta{Present: true, ChannelCount: 2, SampleRate: 48_000 << 16}}
	a2 := &model.Track{ID: 3, Kind: model.KindAudio, Timescale: 44_100,
		Audio: model.AudioMeta{Present: true, ChannelCount: 1, SampleRate: 44_100 << 16}}
	return &model.Timeline{MovieTimescale: 1000, Tracks: []*model.Track{video, a1, a2}}
}

func TestAlignClipsRoundingReported(t *testing.T) {
	tl := buildTimeline()
	// Request a point halfway between sample 1 (41.67ms) and sample 2 (83.33ms).
	clips := []model.ClipPoint{{Label: "cut", TrackID: 1, AtNS: 60_000_000}}
	rep := AlignClips(tl, clips)
	if len(rep.Points) != 1 {
		t.Fatalf("points = %d", len(rep.Points))
	}
	p := rep.Points[0]
	if p.SampleIndex != 1 { // tie breaks toward earlier sample
		t.Errorf("snapped index = %d, want 1", p.SampleIndex)
	}
	if p.RoundingNS == 0 {
		t.Error("non-integral boundary must report rounding")
	}
	if len(rep.NonIntegralRounding()) != 1 {
		t.Error("rounding point not surfaced")
	}
	if len(rep.Faults) != 0 {
		t.Errorf("unexpected faults: %+v", rep.Faults)
	}
}

func TestAlignClipsExactBoundary(t *testing.T) {
	tl := buildTimeline()
	clips := []model.ClipPoint{{Label: "exact", TrackID: 1, AtNS: 2 * 41_666_666}}
	rep := AlignClips(tl, clips)
	if rep.Points[0].RoundingNS != 0 || rep.Points[0].SampleIndex != 2 {
		t.Errorf("exact boundary wrong: %+v", rep.Points[0])
	}
}

func TestAlignClipsOutOfBounds(t *testing.T) {
	tl := buildTimeline()
	clips := []model.ClipPoint{{Label: "future", TrackID: 1, AtNS: 9_999_999_999}}
	rep := AlignClips(tl, clips)
	if len(rep.Faults) != 1 || rep.Faults[0].Kind != model.FaultClipOutOfBounds {
		t.Fatalf("faults = %+v", rep.Faults)
	}
	if !rep.Faults[0].HasTime {
		t.Error("out-of-bounds fault must carry time")
	}
}

func TestAlignClipsUnknownTrack(t *testing.T) {
	tl := buildTimeline()
	rep := AlignClips(tl, []model.ClipPoint{{Label: "x", TrackID: 99, AtNS: 1}})
	if len(rep.Faults) != 1 {
		t.Fatalf("faults = %+v", rep.Faults)
	}
}

func TestNormalizeAudio(t *testing.T) {
	rep, ref := NormalizeAudio(buildTimeline())
	if ref == nil || ref.Channels != 2 || ref.SampleRate != 48_000 {
		t.Fatalf("reference = %+v", ref)
	}
	if len(rep.Audio) != 2 {
		t.Fatalf("audio reports = %d", len(rep.Audio))
	}
	second := rep.Audio[1]
	if second.OriginalRateHz != 44_100 || second.AdoptedRateHz != 48_000 {
		t.Errorf("rate normalization wrong: %+v", second)
	}
	if second.Strategy == "match_reference" || second.Strategy == "" {
		t.Errorf("strategy must describe resample/downmix, got %q", second.Strategy)
	}
}

func TestNormalizeColourMissingAndConflict(t *testing.T) {
	tl := buildTimeline()
	tl.Tracks[0].Video = model.VideoMeta{
		Present: true, HasColr: true, ColourPrimaries: 9,
		TransferCharacteristics: 16, MatrixCoefficients: 9,
	}
	// Second video track with conflicting primaries and no full-range flag.
	other := &model.Track{ID: 4, Kind: model.KindVideo,
		Video: model.VideoMeta{Present: true, HasColr: true, ColourPrimaries: 1,
			TransferCharacteristics: 16, MatrixCoefficients: 9}}
	tl.Tracks = append(tl.Tracks, other)
	// Third video track missing colr entirely.
	missing := &model.Track{ID: 5, Kind: model.KindVideo,
		Video: model.VideoMeta{Present: true}}
	tl.Tracks = append(tl.Tracks, missing)

	rep := NormalizeColour(tl)
	if rep.MissingColour != 4 {
		t.Errorf("missing count = %d, want 4", rep.MissingColour)
	}
	if rep.ConflictColour == 0 {
		t.Error("conflicting primaries must be counted")
	}
	var found bool
	for _, c := range rep.Colour {
		for _, name := range c.FieldConflicts {
			if name != "" && contains(name, "primaries") {
				found = true
			}
		}
	}
	if !found {
		t.Error("primaries conflict detail missing")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
