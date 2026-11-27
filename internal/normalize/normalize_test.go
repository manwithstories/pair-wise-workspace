package normalize

import (
	"testing"

	"fastpost/internal/model"
)

func TestGroupAudioKeepsLayoutsAndRatesDistinct(t *testing.T) {
	tracks := []AudioDesc{
		{Track: "cam-a-stereo-48", Channels: 2, SampleRate: 48000},
		{Track: "cam-b-stereo-441", Channels: 2, SampleRate: 44100},
		{Track: "cam-c-51-48", Channels: 6, SampleRate: 48000},
	}
	groups, err := GroupAudio(tracks)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3 (layout/rate must not be merged)", len(groups))
	}
	// Each group must preserve its exact rate — proof there is no resampling.
	for _, g := range groups {
		for _, tr := range g.Tracks {
			if tr.SampleRate != g.SampleRate {
				t.Fatalf("track %s rate %d != group rate %d (resampled!)",
					tr.Track, tr.SampleRate, g.SampleRate)
			}
		}
	}
}

func TestGroupAudioMergesMatchingTracks(t *testing.T) {
	tracks := []AudioDesc{
		{Track: "a", Channels: 2, SampleRate: 48000},
		{Track: "b", Channels: 2, SampleRate: 48000},
	}
	groups, err := GroupAudio(tracks)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Tracks) != 2 {
		t.Fatalf("matching tracks must share one group, got %+v", groups)
	}
	if groups[0].Layout != LayoutStereo || groups[0].RenditionChannels() != 2 {
		t.Fatalf("layout/channels wrong: %+v", groups[0])
	}
}

func TestGroupAudioIllegalChannelCountFails(t *testing.T) {
	tracks := []AudioDesc{
		{Track: "good", Channels: 2, SampleRate: 48000},
		{Track: "bad", Channels: 7, SampleRate: 48000},
	}
	_, err := GroupAudio(tracks)
	if err == nil {
		t.Fatal("7-channel track must fail")
	}
	faults := inputFaults(t, err)
	if faults[0].Kind != model.FaultChannelMapping || faults[0].Track != "bad" {
		t.Fatalf("fault = %+v, want illegal-channel-mapping on bad", faults[0])
	}
}

func TestGroupAudioUnsupportedRateFails(t *testing.T) {
	_, err := GroupAudio([]AudioDesc{
		{Track: "weird", Channels: 2, SampleRate: 22050},
	})
	faults := inputFaults(t, err)
	if faults[0].Kind != model.FaultChannelMapping {
		t.Fatalf("fault = %+v, want illegal-channel-mapping", faults[0])
	}
}

func TestColorGroupsOnConflict(t *testing.T) {
	descs := []ColorDesc{
		{Track: "sdr1", Primaries: "bt709", VideoRange: "SDR"},
		{Track: "hdr1", Primaries: "bt2020", Transfer: "smpte2084", VideoRange: "PQ"},
		{Track: "sdr2", Primaries: "bt709", VideoRange: "SDR"},
	}
	if !Conflicts(descs) {
		t.Fatal("distinct signatures must be reported as conflict")
	}
	groups, err := GroupColor(descs, ColorGroupOnConflict)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 under group policy", len(groups))
	}
	// Group ordering is deterministic.
	if groups[0].ID > groups[1].ID {
		t.Fatal("color groups not stably sorted")
	}
}

func TestColorRejectOnConflict(t *testing.T) {
	descs := []ColorDesc{
		{Track: "sdr1", Primaries: "bt709", VideoRange: "SDR"},
		{Track: "hdr1", Primaries: "bt2020", VideoRange: "PQ"},
	}
	_, err := GroupColor(descs, ColorRejectOnConflict)
	faults := inputFaults(t, err)
	if faults[0].Kind != model.FaultColorConflict || faults[0].Track != "hdr1" {
		t.Fatalf("fault = %+v, want color-conflict on hdr1", faults[0])
	}
}

func TestColorNoConflictSingleGroup(t *testing.T) {
	descs := []ColorDesc{
		{Track: "a", Primaries: "bt709", VideoRange: "SDR"},
		{Track: "b", Primaries: "bt709", VideoRange: "SDR"},
	}
	if Conflicts(descs) {
		t.Fatal("identical signatures are not a conflict")
	}
	groups, err := GroupColor(descs, ColorRejectOnConflict)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Tracks) != 2 {
		t.Fatalf("want one merged group, got %+v", groups)
	}
}

func inputFaults(t *testing.T, err error) []model.Fault {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	in, ok := err.(*model.InputError)
	if !ok {
		t.Fatalf("err type %T, want *InputError", err)
	}
	return in.Faults
}
