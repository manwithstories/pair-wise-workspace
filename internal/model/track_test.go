package model

import (
	"errors"
	"fmt"
	"testing"
)

func TestTrackByteAndSampleCounts(t *testing.T) {
	tr := Track{Timescale: 90000, Samples: []Sample{
		{Index: 0, Size: 100, DTS: 0, Duration: 3000},
		{Index: 1, Size: 250, DTS: 3000, Duration: 3000},
	}}
	if tr.SampleCount() != 2 || tr.ByteCount() != 350 {
		t.Fatalf("counts = %d/%d", tr.SampleCount(), tr.ByteCount())
	}
	if tr.DurationTicks() != 6000 {
		t.Fatalf("duration = %d", tr.DurationTicks())
	}
	num, den := tr.MediaDurationNum()
	if num != 6000 || den != 90000 {
		t.Fatalf("media duration = %d/%d", num, den)
	}
}

func TestCodedErrorFormattingAndExit(t *testing.T) {
	err := NewError(ErrTruncatedBox, 7, 4096, "moov child overruns input")
	var ce *CodedError
	if !errors.As(err, &ce) {
		t.Fatal("not a coded error")
	}
	if ce.Code != ErrTruncatedBox || ce.Track != 7 || ce.Offset != 4096 {
		t.Fatalf("unexpected error %+v", ce)
	}
	want := "TRUNCATED_BOX: track 7 offset 4096: moov child overruns input"
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if ExitCode(err) != ExitDataError {
		t.Fatalf("exit = %d", ExitCode(err))
	}
	if ExitCode(NewError(ErrParamInvalid, 0, 0, "bad flag")) != ExitParamError {
		t.Fatal("param error should exit 1")
	}
	if ExitCode(NewError(ErrCheckpointCorrupt, 0, 0, "index")) != ExitUnrecoverable {
		t.Fatal("checkpoint corrupt should exit 3")
	}
	if ExitCode(nil) != ExitOK {
		t.Fatal("nil should exit 0")
	}
	if ExitCode(fmt.Errorf("plain")) != ExitDataError {
		t.Fatal("plain error should be data error")
	}
	if CodeOf(fmt.Errorf("plain")) != ErrUnrecoverable {
		t.Fatal("plain error code should be unrecoverable")
	}
}

func TestTrackKindAndLayer(t *testing.T) {
	for _, k := range []TrackKind{KindVideo, KindAudio, KindData} {
		if !k.Valid() {
			t.Fatalf("%s should be valid", k)
		}
	}
	if TrackKind("subtitle").Valid() {
		t.Fatal("subtitle should not be valid")
	}
	if LayerExplicit <= LayerTrack || LayerTrack <= LayerContainer {
		t.Fatal("layer order broken")
	}
	if LayerExplicit.String() != "explicit" || MetaLayer(9).String() != "unknown" {
		t.Fatal("layer string broken")
	}
}

func TestDisplayMetaCloneIsDeep(t *testing.T) {
	c := &Colour{Primaries: 1, Known: true}
	m := DisplayMeta{Rotation: 90, Colour: c, Claims: []Claim{{Field: "rotation"}}, Warnings: []string{"w"}}
	n := m.Clone()
	n.Colour.Primaries = 9
	n.Claims[0].Field = "changed"
	n.Warnings[0] = "changed"
	if m.Colour.Primaries != 1 || m.Claims[0].Field != "rotation" || m.Warnings[0] != "w" {
		t.Fatal("clone aliased original")
	}
}
