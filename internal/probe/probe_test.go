package probe

import (
	"errors"
	"testing"
)

func audioRep(id string, entry, mask, layout, rate, ts int) Representation {
	return Representation{
		ID: id,
		Audio: &AudioInfo{
			TrackID:       1,
			EntryChannels: entry,
			Mask:          uint64(mask),
			HasMask:       mask != 0,
			LayoutCount:   layout,
			SampleRate:    rate,
			Timescale:     ts,
			ObjectType:    0x40,
			HasObjectType: true,
		},
	}
}

func videoRep(id string, p, t, m uint16) Representation {
	return Representation{
		ID: id,
		Video: &VideoInfo{
			TrackID: 2, Width: 1920, Height: 1080,
			Color: ColorInfo{Present: true, Primaries: p, Transfer: t, Matrix: m, Complete: true},
		},
	}
}

// 5.1 layout bit mask: FL FR FC LFE BL BR.
const mask51 = 0x3F

func TestAudioMaskMatchesEntry(t *testing.T) {
	rep, err := Validate([]Representation{audioRep("a", 6, mask51, 6, 48000, 48000)}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	a := rep.Audio[0]
	if a.Channels != 6 || a.Mask != mask51 || a.SampleRate != 48000 {
		t.Fatalf("audio=%+v", a)
	}
	if rep.Failed() {
		t.Fatalf("unexpected error findings: %v", rep.Findings)
	}
}

func TestAudioMaskConflictNormalisedByMask(t *testing.T) {
	// Entry claims 2 channels, mask carries 6. Default policy is Mask.
	rep, err := Validate([]Representation{audioRep("a", 2, mask51, 6, 48000, 48000)}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Audio[0].Channels != 6 {
		t.Fatalf("channels=%d want mask popcount 6", rep.Audio[0].Channels)
	}
	assertFinding(t, rep, "CHANNEL_COUNT_CONFLICT", Warning)
}

func TestAudioMaskConflictRejected(t *testing.T) {
	p := DefaultPolicy()
	p.Channels = Reject
	_, err := Validate([]Representation{audioRep("a", 2, mask51, 6, 48000, 48000)}, p)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v want rejected", err)
	}
}

func TestAudioUnknownMaskPreserved(t *testing.T) {
	rep, err := Validate([]Representation{audioRep("a", 6, 0, 0, 48000, 48000)}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Audio[0].Channels != 6 {
		t.Fatalf("entry count should be preserved: %+v", rep.Audio[0])
	}
	assertFinding(t, rep, "UNKNOWN_CHANNEL_MASK", Warning)
}

func TestAudioSampleRateConflict(t *testing.T) {
	// Entry says 44100, timescale 48000. Default prefers the entry value.
	rep, err := Validate([]Representation{audioRep("a", 2, 0x3, 2, 44100, 48000)}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Audio[0].SampleRate != 44100 {
		t.Fatalf("rate=%d", rep.Audio[0].SampleRate)
	}
	assertFinding(t, rep, "SAMPLE_RATE_CONFLICT", Warning)

	p := DefaultPolicy()
	p.SampleRate = Timescale
	rep2, err := Validate([]Representation{audioRep("a", 2, 0x3, 2, 44100, 48000)}, p)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Audio[0].SampleRate != 48000 {
		t.Fatalf("timescale policy rate=%d", rep2.Audio[0].SampleRate)
	}
}

func TestAudioUnknownRatePreserved(t *testing.T) {
	rep, err := Validate([]Representation{audioRep("a", 2, 0x3, 2, 12345, 12345)}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Audio[0].SampleRate != 12345 || rep.Audio[0].SampleRateKnown {
		t.Fatalf("unknown rate must be preserved verbatim: %+v", rep.Audio[0])
	}
	assertFinding(t, rep, "UNKNOWN_SAMPLE_RATE", Warning)
}

func TestVideoColorConflictFirstWins(t *testing.T) {
	reps := []Representation{
		videoRep("v0", 1, 1, 1),
		videoRep("v1", 9, 16, 9),
	}
	rep, err := Validate(reps, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Video) != 1 || rep.Video[0].Color.Primaries != 1 {
		t.Fatalf("video=%+v", rep.Video)
	}
	assertFinding(t, rep, "COLOR_CONFLICT", Warning)
}

func TestVideoColorConflictRejected(t *testing.T) {
	p := DefaultPolicy()
	p.Color = Reject
	reps := []Representation{videoRep("v0", 1, 1, 1), videoRep("v1", 9, 16, 9)}
	if _, err := Validate(reps, p); !errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v want rejected", err)
	}
}

func TestVideoUnknownColorPreserved(t *testing.T) {
	rep, err := Validate([]Representation{videoRep("v0", 99, 98, 97)}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Video[0].Color.Primaries != 99 {
		t.Fatalf("unknown colour must be preserved: %+v", rep.Video[0].Color)
	}
	if rep.Video[0].ColorKnown {
		t.Fatal("unknown colour must not be marked known")
	}
	assertFinding(t, rep, "UNKNOWN_PRIMARIES", Warning)
	assertFinding(t, rep, "UNKNOWN_TRANSFER", Warning)
	assertFinding(t, rep, "UNKNOWN_MATRIX", Warning)
}

func TestVideoMissingColor(t *testing.T) {
	rep, err := Validate([]Representation{{ID: "v0", Video: &VideoInfo{TrackID: 2, Width: 640, Height: 480}}}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	assertFinding(t, rep, "MISSING_COLOR", Warning)
}

func TestNoTracks(t *testing.T) {
	if _, err := Validate([]Representation{{ID: "empty"}}, DefaultPolicy()); !errors.Is(err, ErrNoTrack) {
		t.Fatalf("err=%v", err)
	}
}

func TestAudioCrossRepresentation(t *testing.T) {
	reps := []Representation{
		audioRep("a0", 6, mask51, 6, 48000, 48000),
		audioRep("a1", 2, 0x3, 2, 44100, 44100),
	}
	rep, err := Validate(reps, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Audio) != 1 || rep.Audio[0].SampleRate != 48000 || rep.Audio[0].Mask != mask51 {
		t.Fatalf("audio=%+v", rep.Audio)
	}
	assertFinding(t, rep, "SAMPLE_RATE_CONFLICT", Warning)
	assertFinding(t, rep, "CHANNEL_MASK_CONFLICT", Warning)
}

func assertFinding(t *testing.T, r Report, code string, sev Severity) {
	t.Helper()
	for _, f := range r.Findings {
		if f.Code == code && f.Severity == sev {
			return
		}
	}
	t.Fatalf("missing finding %s/%s in %v", code, sev, r.Findings)
}
