package ingest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/example/mediad/internal/model"
)

func mxfSpec(path string) MXFFixtureSpec {
	return MXFFixtureSpec{
		Path:              path,
		Width:             1920,
		Height:            1080,
		EditRate:          [2]uint32{30, 1},
		VideoSamples:      12,
		KeyframeEvery:     3,
		ContainerRotation: 90,
		TrackRotation:     270,
		ContainerCrop:     &Crop{0, 0, 1600, 900},
		TrackCrop:         &Crop{10, 20, 1280, 720},
		ContainerColor:    &Color{Primaries: 1, Transfer: 1, Matrix: 1},
		TrackColor:        &Color{Primaries: 1, Transfer: 1, Matrix: 1},
		AudioSampleRate:   48000,
		AudioChannels:     2,
		AudioLayout:       []string{"L", "R"},
	}
}

func TestGenerateAndParseMXF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cam1.mxf")
	if _, err := GenerateMXF(mxfSpec(path)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := ParseMXF(bytes.NewReader(b), path)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Format != "mxf" || len(sum.Tracks) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	var vid, aud *Track
	for _, tr := range sum.Tracks {
		switch tr.Kind {
		case "video":
			vid = tr
		case "audio":
			aud = tr
		}
	}
	if vid.Video.Width != 1920 || vid.Video.Height != 1080 {
		t.Fatalf("dimensions = %dx%d", vid.Video.Width, vid.Video.Height)
	}
	if vid.Video.SampleCount != 12 || len(vid.Video.KeyframeIndexes) != 4 {
		t.Fatalf("samples=%d keys=%v", vid.Video.SampleCount,
			vid.Video.KeyframeIndexes)
	}
	if vid.ContainerRot != 90 || vid.TrackRot != 270 {
		t.Fatalf("rot = %d/%d", vid.ContainerRot, vid.TrackRot)
	}
	if vid.ContainerCrop.Width != 1600 || vid.Video.Crop.Width != 1280 {
		t.Fatalf("crops = %+v / %+v", vid.ContainerCrop, vid.Video.Crop)
	}
	if len(vid.Video.Colors) != 2 {
		t.Fatalf("colors = %+v", vid.Video.Colors)
	}
	if aud.Audio.SampleRate != 48000 || aud.Audio.Channels != 2 ||
		len(aud.Audio.ChannelLayout) != 2 {
		t.Fatalf("audio = %+v", aud.Audio)
	}

	if err := Adjudicate(sum, DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
	if vid.AppliedRotation != 270 || vid.AppliedCrop.Width != 1280 {
		t.Fatalf("applied rot=%d crop=%+v", vid.AppliedRotation, vid.AppliedCrop)
	}
}

func TestMXFCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.mxf")
	spec := mxfSpec(path)
	spec.Corrupt = true
	if _, err := GenerateMXF(spec); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	_, err := ParseMXF(bytes.NewReader(b), path)
	if err == nil {
		t.Fatal("corrupt MXF must be rejected")
	}
	me, ok := model.AsError(err)
	if !ok || me.Kind != model.KindCorrupt {
		t.Fatalf("wrong error: %v", err)
	}

	// Truncated KLV payload must fail too.
	trunc := append([]byte(nil), b[:len(b)-6]...)
	if _, err := ParseMXF(bytes.NewReader(trunc), path); err == nil {
		t.Fatal("truncated MXF must be rejected")
	}
}

func TestMXFRejectsBadMagic(t *testing.T) {
	bad := bytes.Repeat([]byte{0xaa}, 32)
	if _, err := ParseMXF(bytes.NewReader(bad), "x.mxf"); err == nil {
		t.Fatal("bad magic must be rejected")
	}
}
