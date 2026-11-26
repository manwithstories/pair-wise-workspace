package audio

import (
	"testing"

	"mediad/internal/media"
)

func TestValidateRepairsCorroboratedRateAndChannelConflict(t *testing.T) {
	tr := &media.Track{ID: 2, Kind: media.KindAudio, Timescale: 48_000, Audio: &media.AudioMetadata{
		SampleRate: 22_050, ChannelCount: 1, DeclaredRate: 48_000, DeclaredChannels: 2,
		Layout: []string{"L", "R"},
	}}
	rep := Validate(tr)
	if !rep.OK || rep.Repairs != 2 {
		t.Fatalf("report=%#v", rep)
	}
	if tr.Audio.SampleRate != 48_000 || tr.Audio.ChannelCount != 2 || len(tr.Audio.Layout) != 2 {
		t.Fatalf("audio not repaired: %#v", tr.Audio)
	}
	if len(tr.Faults) != 2 {
		t.Fatalf("faults should preserve both conflicts: %#v", tr.Faults)
	}
	for _, f := range tr.Faults {
		if f.Original == "" || f.RepairBasis == "" {
			t.Fatalf("fault lacks original/basis: %#v", f)
		}
	}
}

func TestValidateInfersCanonicalLayout(t *testing.T) {
	tr := &media.Track{ID: 2, Kind: media.KindAudio, Timescale: 48_000, Audio: &media.AudioMetadata{SampleRate: 48_000, ChannelCount: 6}}
	rep := Validate(tr)
	if !rep.OK || len(tr.Audio.Layout) != 6 {
		t.Fatalf("rep=%#v layout=%v", rep, tr.Audio.Layout)
	}
}

func TestValidateFailsAmbiguousConflict(t *testing.T) {
	tr := &media.Track{ID: 3, Kind: media.KindAudio, Timescale: 48_000, Audio: &media.AudioMetadata{
		SampleRate: 44_100, ChannelCount: 2, Layout: []string{"L", "R", "C"},
	}}
	rep := Validate(tr)
	if rep.OK {
		t.Fatal("expected ambiguous layout failure")
	}
	if len(rep.Faults) == 0 || rep.Faults[0].Recoverable {
		t.Fatalf("faults should be non-recoverable: %#v", rep.Faults)
	}
}

func TestValidateMissingMetadataFailsTrack(t *testing.T) {
	tr := &media.Track{ID: 4, Kind: media.KindAudio, Timescale: 48_000}
	if rep := Validate(tr); rep.OK {
		t.Fatal("expected missing audio metadata failure")
	}
}
