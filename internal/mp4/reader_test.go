package mp4

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediasync/internal/fixtures"
	"mediasync/internal/model"
)

func writeFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	p, err := fixtures.WriteFile(dir, name, data)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadFixtureTracksAndHashes(t *testing.T) {
	b, err := fixtures.BuildMP4(fixtures.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	p := writeFixture(t, "studio.mp4", b.Data)
	in, err := ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(in.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(in.Tracks))
	}
	if !in.FastStart {
		t.Fatal("fixture should be fast-start")
	}
	byID := map[uint32]*model.Track{}
	for _, tr := range in.Tracks {
		byID[tr.ID] = tr
	}
	v := byID[1]
	if v == nil || v.Kind != model.KindVideo {
		t.Fatalf("video track missing: %+v", v)
	}
	if len(v.Samples) != len(b.VideoSizes) {
		t.Fatalf("video samples = %d, want %d", len(v.Samples), len(b.VideoSizes))
	}
	if v.Codec != "avc1" || v.Width != 1920 || v.Height != 1080 {
		t.Fatalf("video metadata = %s %dx%d", v.Codec, v.Width, v.Height)
	}
	if v.Meta.Rotation != 90 || v.Meta.RotationSrc != model.SourceMP4 {
		t.Fatalf("rotation = %d from %s", v.Meta.Rotation, v.Meta.RotationSrc)
	}
	// Verify coded bytes really are the fixture payloads, byte for byte.
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i, s := range v.Samples {
		buf := make([]byte, s.Size)
		if _, err := f.ReadAt(buf, s.Offset); err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if string(buf) != string(b.VideoPayloads[i]) {
			t.Fatalf("sample %d payload mismatch", i)
		}
	}
	a := byID[2]
	if a == nil || a.Kind != model.KindAudio {
		t.Fatalf("audio track missing")
	}
	if a.SampleRate != 48000 || a.Channels != 2 {
		t.Fatalf("audio = %d Hz %d ch", a.SampleRate, a.Channels)
	}
	if a.Samples[0].Duration != 1024 {
		t.Fatalf("audio sample duration = %d", a.Samples[0].Duration)
	}

	// Stats hashes must match the fixture's own hashes.
	for _, st := range in.Stats {
		switch st.TrackID {
		case 1:
			if st.SHA256 != b.VideoHash || st.Samples != len(b.VideoSizes) {
				t.Fatalf("video stats mismatch")
			}
		case 2:
			if st.SHA256 != b.AudioHash || st.Samples != len(b.AudioSizes) {
				t.Fatalf("audio stats mismatch")
			}
		}
	}
}

func TestReadTruncatedReportsOffsetAndCode(t *testing.T) {
	b, err := fixtures.BuildMP4Truncated(fixtures.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	p := writeFixture(t, b.Name, b.Data)
	_, err = ReadFile(p)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	var ce *model.CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("not coded: %v", err)
	}
	if ce.Code != model.ErrTruncatedBox {
		t.Fatalf("code = %s", ce.Code)
	}
	if ce.Offset == 0 {
		t.Fatalf("offset should be reported, got %d", ce.Offset)
	}
	if int(ce.Offset) > len(b.Data) {
		t.Fatalf("offset %d past input %d", ce.Offset, len(b.Data))
	}
	if model.ExitCode(err) != model.ExitDataError {
		t.Fatalf("exit = %d", model.ExitCode(err))
	}
}

func TestReadRejectsCropOutOfBounds(t *testing.T) {
	b, err := fixtures.BuildMP4CropOutOfBounds(fixtures.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	p := writeFixture(t, b.Name, b.Data)
	_, err = ReadFile(p)
	if err == nil {
		t.Fatal("expected crop rejection")
	}
	if model.CodeOf(err) != model.ErrCropOutOfBounds {
		t.Fatalf("code = %s", model.CodeOf(err))
	}
}

func TestReadUnknownColourPreserved(t *testing.T) {
	b, err := fixtures.BuildMP4UnknownColour(fixtures.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	p := writeFixture(t, b.Name, b.Data)
	in, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var v *model.Track
	for _, tr := range in.Tracks {
		if tr.Kind == model.KindVideo {
			v = tr
		}
	}
	if v == nil || v.Meta.Colour == nil {
		t.Fatal("colour missing")
	}
	c := v.Meta.Colour
	if c.Known {
		t.Fatal("unknown colour must not be marked known")
	}
	if c.Primaries != 0xffff || c.Transfer != 0xfffe || c.Matrix != 0xfffd {
		t.Fatalf("unknown colour values not preserved: %+v", c)
	}
	if c.DefaultInterpretation != "bt709" {
		t.Fatalf("default interpretation = %q", c.DefaultInterpretation)
	}
	found := false
	for _, w := range v.Meta.Warnings {
		if len(w) > 0 && w[0] > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a warning for unknown colour")
	}
}

func TestReadMissingFile(t *testing.T) {
	_, err := ReadFile(filepath.Join(t.TempDir(), "nope.mp4"))
	if err == nil {
		t.Fatal("expected open error")
	}
}

func TestRotationMatrixDecoding(t *testing.T) {
	cases := []struct {
		m    [9]int32
		want int
		ok   bool
	}{
		{[9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}, 0, true},
		{[9]int32{0, 0x10000, 0, -0x10000, 0, 0, 0, 0, 0x40000000}, 90, true},
		{[9]int32{-0x10000, 0, 0, 0, -0x10000, 0, 0, 0, 0x40000000}, 180, true},
		{[9]int32{0, -0x10000, 0, 0x10000, 0, 0, 0, 0, 0x40000000}, 270, true},
		{[9]int32{0x8000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}, 0, false},
	}
	for i, c := range cases {
		got, ok := rotationFromMatrix(c.m)
		if ok != c.ok || got != c.want {
			t.Fatalf("case %d: got %d/%v want %d/%v", i, got, ok, c.want, c.ok)
		}
	}
}
