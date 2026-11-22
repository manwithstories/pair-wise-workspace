package testdata

import (
	"bytes"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

func TestSyntheticMP4Probes(t *testing.T) {
	f, err := Build(DefaultParams(17))
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(f.MP4)
	info, err := mp4.Probe(r)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(info.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(info.Tracks))
	}
	video := info.Tracks[0]
	if video.TrackID != f.Video.TrackID || video.Timescale != f.Video.Timescale {
		t.Fatalf("video track mismatch: %+v", video)
	}
	if len(video.Samples) != len(f.Video.Samples) {
		t.Fatalf("video samples = %d, want %d", len(video.Samples), len(f.Video.Samples))
	}
	if len(video.Chunks) != len(f.Video.Samples) {
		t.Fatalf("chunks = %d, want one per sample", len(video.Chunks))
	}
	audio := info.Tracks[1]
	if len(audio.Samples) != len(f.Audio.Samples) {
		t.Fatalf("audio samples = %d", len(audio.Samples))
	}

	// VFR fixture: stts must preserve multiple distinct sample deltas.
	pv := DefaultParams(17)
	pv.VFR = true
	fv, err := Build(pv)
	if err != nil {
		t.Fatal(err)
	}
	infoV, err := mp4.Probe(bytes.NewReader(fv.MP4))
	if err != nil {
		t.Fatal(err)
	}
	distinct := map[uint32]bool{}
	for _, s := range infoV.Tracks[0].Samples {
		distinct[s.TimeDelta] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("VFR not preserved: only %d distinct delta(s)", len(distinct))
	}
}
