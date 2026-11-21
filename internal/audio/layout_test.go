package audio

import (
	"testing"

	"mkvarchive/internal/model"
)

func audioTrack(uid uint64, channels uint64, rate float64) *model.Track {
	return &model.Track{UID: uid, Kind: model.KindAudio, HeaderOffset: int64(uid * 10),
		Audio: &model.AudioMeta{Channels: channels, SamplingFrequency: rate}}
}

func TestNormalizeLayouts(t *testing.T) {
	cases := map[uint64]Layout{
		1: LayoutMono, 2: LayoutStereo, 6: LayoutSurround51, 8: LayoutSurround71, 7: LayoutUnknown,
	}
	for ch, want := range cases {
		if got := NormalizeLayout(ch); got != want {
			t.Fatalf("channels %d: got %s want %s", ch, got, want)
		}
	}
}

func TestRejectPolicyBlocksSampleRateConflict(t *testing.T) {
	tracks := []*model.Track{audioTrack(1, 2, 48000), audioTrack(2, 2, 44100)}
	_, _, err := ValidateTracks(tracks, PolicyReject)
	ae, ok := err.(*model.ArchiveError)
	if !ok || ae.Code != "AUDIO_SAMPLE_RATE_CONFLICT" {
		t.Fatalf("want rate conflict error, got %v", err)
	}
}

func TestRetainPolicyReportsPerTrackRates(t *testing.T) {
	tracks := []*model.Track{audioTrack(1, 2, 48000), audioTrack(2, 3, 44100)}
	reports, warnings, err := ValidateTracks(tracks, PolicyRetain)
	if err != nil || len(reports) != 2 {
		t.Fatalf("retain must succeed: %v reports=%d", err, len(reports))
	}
	var sawRate, sawLayout bool
	for _, w := range warnings {
		if w.Code == "AUDIO_SAMPLE_RATE_CONFLICT" {
			sawRate = true
		}
		if w.Code == "AUDIO_UNKNOWN_LAYOUT" {
			sawLayout = true
		}
	}
	if !sawRate || !sawLayout {
		t.Fatalf("missing warnings: rate=%v layout=%v all=%+v", sawRate, sawLayout, warnings)
	}
	if reports[0].NormalizedLayout != LayoutStereo ||
		reports[1].NormalizedLayout != LayoutUnknown {
		t.Fatalf("layout normalization wrong: %+v", reports)
	}
}

func TestChannelPositionsMismatch(t *testing.T) {
	// 6 declared channels but only 2 float32 angles (8 bytes) in the table.
	tr := audioTrack(5, 6, 48000)
	tr.Audio.HasChannelPositions = true
	tr.Audio.ChannelPositions = make([]byte, 8)
	if _, _, err := ValidateTracks([]*model.Track{tr}, PolicyReject); err == nil {
		t.Fatal("positions/channel mismatch must be rejected under strict policy")
	}
	tr.Audio.ChannelPositions = make([]byte, 24) // 6 float32 angles
	reports, _, err := ValidateTracks([]*model.Track{tr}, PolicyRetain)
	if err != nil || len(reports) != 1 || reports[0].NormalizedLayout != LayoutSurround51 {
		t.Fatalf("matching positions should pass: %v %+v", err, reports)
	}
}

func TestSurroundLayoutsRecordRawAndNormalized(t *testing.T) {
	tracks := []*model.Track{
		audioTrack(1, 6, 48000),
		audioTrack(2, 8, 48000),
	}
	reports, _, err := ValidateTracks(tracks, PolicyRetain)
	if err != nil {
		t.Fatalf("surround layouts should pass: %v", err)
	}
	if reports[0].DeclaredChannels != 6 || reports[0].NormalizedLayout != LayoutSurround51 {
		t.Fatalf("5.1 report wrong: %+v", reports[0])
	}
	if reports[1].DeclaredChannels != 8 || reports[1].NormalizedLayout != LayoutSurround71 {
		t.Fatalf("7.1 report wrong: %+v", reports[1])
	}
}

func TestNonAudioTracksIgnored(t *testing.T) {
	video := &model.Track{UID: 9, Kind: model.KindVideo}
	reports, warnings, err := ValidateTracks([]*model.Track{video}, PolicyReject)
	if err != nil || len(reports) != 0 || len(warnings) != 0 {
		t.Fatalf("video track must be ignored: %v %+v", err, reports)
	}
}
