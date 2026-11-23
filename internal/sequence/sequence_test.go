package sequence

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"medpipe/internal/model"
)

func fixture(t *testing.T) ([]model.Track, map[string][]model.Frame) {
	t.Helper()
	tracks := []model.Track{
		{ID: "cam0", Kind: model.KindVideo, Timescale: 90000, ClockBits: 16, NominalTickDelta: 3000,
			Extra: []model.Metadata{{Key: "lens", Value: "35mm"}}},
		{ID: "mic0", Kind: model.KindAudio, Timescale: 48000, SampleRate: 48000, ClockBits: 16, NominalTickDelta: 1024},
	}
	frames := map[string][]model.Frame{"cam0": {}, "mic0": {}}
	for i := 0; i < 12; i++ {
		fr := model.Frame{
			TrackID: "cam0", Seq: uint64(i), RawTS: uint64(64000 + i*3000),
			Keyframe: i%4 == 0, Width: 1920, Height: 1080,
			Payload: bytes.Repeat([]byte{byte(i + 1)}, 64+i),
			Meta:    []model.Metadata{{Key: "scene", Value: "wide"}},
		}
		fr.Checksum = fr.IntegrityChecksum()
		frames["cam0"] = append(frames["cam0"], fr)
	}
	for i := 0; i < 5; i++ {
		fr := model.Frame{
			TrackID: "mic0", Seq: uint64(i), RawTS: uint64(65000 + i*1024),
			Energy:  []float64{0.02 * float64(i), 0.5},
			Payload: []byte{0xaa, byte(i)},
		}
		fr.Checksum = fr.IntegrityChecksum()
		frames["mic0"] = append(frames["mic0"], fr)
	}
	return tracks, frames
}

func TestRoundTripDigest(t *testing.T) {
	tracks, frames := fixture(t)
	path := filepath.Join(t.TempDir(), "seq", "capture.medpseq")
	digest, size, err := WriteFile(path, tracks, frames)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile("", path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != digest {
		t.Fatalf("digest mismatch: %x vs %x", got.Digest, digest)
	}
	if got.Size != size {
		t.Fatalf("size mismatch: %d vs %d", got.Size, size)
	}
	want := DigestFrames(tracks, frames)
	if got := DigestFrames(got.Header.Tracks, got.Frames); got != want {
		t.Fatalf("frame digest mismatch: %x vs %x", got, want)
	}
	if len(got.Frames["cam0"]) != 12 || len(got.Frames["mic0"]) != 5 {
		t.Fatalf("frame counts = %d/%d", len(got.Frames["cam0"]), len(got.Frames["mic0"]))
	}
	if got.Frames["cam0"][3].Checksum != frames["cam0"][3].Checksum {
		t.Fatal("checksum not preserved")
	}
	if got.Frames["mic0"][2].Energy[1] != 0.5 {
		t.Fatal("energy not preserved")
	}
	if got.Frames["cam0"][0].Meta[0].Value != "wide" {
		t.Fatal("metadata not preserved")
	}
}

func TestChecksumCorruptionReportedWithOffset(t *testing.T) {
	tracks, frames := fixture(t)
	var buf bytes.Buffer
	if _, _, err := WriteTo(&buf, tracks, frames); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	// Flip a payload byte inside the third cam0 record.
	idx := bytes.Index(data, bytes.Repeat([]byte{3}, 66))
	if idx < 0 {
		t.Fatal("could not locate payload")
	}
	data[idx+3] ^= 0xff
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected checksum error")
	}
	if !model.IsKind(err, model.KindCorrupt) {
		t.Fatalf("kind = %v: %v", err, err)
	}
	me, ok := err.(*model.Error)
	if !ok || me.TrackID != "cam0" || me.Offset == 0 {
		t.Fatalf("error lacks track/offset: %+v", err)
	}
	if model.ExitCode(err) != 2 {
		t.Fatalf("exit code = %d, want 2", model.ExitCode(err))
	}
}

func TestSequenceOrderAndStructureRejected(t *testing.T) {
	tracks, frames := fixture(t)
	cases := map[string]func([]byte) []byte{
		"bad magic": func(b []byte) []byte {
			b[0] = 'X'
			return b
		},
		"truncated": func(b []byte) []byte { return b[:len(b)-40] },
		"trailing byte": func(b []byte) []byte {
			return append(b, 0)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, _, err := WriteTo(&buf, tracks, frames); err != nil {
				t.Fatal(err)
			}
			_, err := Parse(mutate(buf.Bytes()))
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !model.IsKind(err, model.KindCorrupt) {
				t.Fatalf("kind = %v: %v", err, err)
			}
		})
	}
}

func TestValidateRejectsNonMonotonicFrames(t *testing.T) {
	_, frames := fixture(t)
	frames["cam0"][5].Seq = frames["cam0"][4].Seq
	err := Validate([]model.Track{{ID: "cam0", Kind: model.KindVideo, Timescale: 90000}},
		map[string][]model.Frame{"cam0": frames["cam0"]})
	if err == nil || !model.IsKind(err, model.KindCorrupt) {
		t.Fatalf("got %v", err)
	}
}

func TestValidateRejectsStaleChecksum(t *testing.T) {
	_, frames := fixture(t)
	frames["cam0"][1].Seq = 99 // mutate after checksum was computed
	err := Validate([]model.Track{{ID: "cam0", Kind: model.KindVideo, Timescale: 90000}},
		map[string][]model.Frame{"cam0": frames["cam0"]})
	if err == nil || !model.IsKind(err, model.KindCorrupt) {
		t.Fatalf("got %v", err)
	}
}

func TestResolveWithinRejectsEscape(t *testing.T) {
	base := t.TempDir()
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../b", ".."} {
		if _, err := ResolveWithin(base, bad); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
	if _, err := ResolveWithin(base, "nested/file.medpseq"); err != nil {
		t.Errorf("valid path rejected: %v", err)
	}
}

func TestWriteFileLeavesNoTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should go makes the final rename fail.
	path := filepath.Join(dir, "blocked")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	tracks, frames := fixture(t)
	if _, _, err := WriteFile(path, tracks, frames); err == nil {
		t.Fatal("expected write failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "blocked" {
			t.Fatalf("leftover temp file %q", e.Name())
		}
	}
}
