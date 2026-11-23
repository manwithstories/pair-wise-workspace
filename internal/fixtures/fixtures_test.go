package fixtures

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPayloadDeterminism(t *testing.T) {
	a := Payload(41, 7, 3, 64)
	b := Payload(41, 7, 3, 64)
	if !bytes.Equal(a, b) {
		t.Fatal("payload is not deterministic for the same seed and index")
	}
	c := Payload(42, 7, 3, 64)
	if bytes.Equal(a, c) {
		t.Fatal("payload should change with the seed")
	}
	if len(Payload(41, 7, 3, 0)) != 0 {
		t.Fatal("zero-size payload should be empty")
	}
}

func TestHashSamplesSensitiveToOrder(t *testing.T) {
	one, err := HashSamples([][]byte{{1, 2}, {3}})
	if err != nil {
		t.Fatal(err)
	}
	two, err := HashSamples([][]byte{{3}, {1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Fatal("hash must depend on sample order")
	}
}

func TestWriteFileRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteFile(dir, "../escape.bin", []byte{1}); err == nil {
		t.Fatal("path escape should be rejected")
	}
	if _, err := WriteFile(dir, "/abs.bin", []byte{1}); err == nil {
		t.Fatal("absolute path should be rejected")
	}
	if _, err := WriteFile(dir, "nested/ok.bin", []byte{1}); err != nil {
		t.Fatalf("nested write should succeed: %v", err)
	}
}

func TestMP4FixtureDeterministic(t *testing.T) {
	a, err := BuildMP4(DemoMP4Options())
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildMP4(DemoMP4Options())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Data, b.Data) {
		t.Fatal("MP4 fixture is not byte-for-byte reproducible")
	}
	if a.VideoHash != b.VideoHash {
		t.Fatal("video hash differs between identical builds")
	}
}

func TestMXFFixtureDeterministic(t *testing.T) {
	a, err := BuildMXF(DefaultMxfOptions())
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildMXF(DefaultMxfOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Data, b.Data) {
		t.Fatal("MXF fixture is not byte-for-byte reproducible")
	}
}

func TestGenerateWritesAllInputs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "input")
	g, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{g.MP4, g.MXF, g.RTP, g.TruncatedMP4, g.InvalidKLV} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
}

func TestInvalidOptionsRejected(t *testing.T) {
	bad := DefaultOptions()
	bad.VideoTrackID = bad.AudioTrackID
	if _, err := BuildMP4(bad); err == nil {
		t.Fatal("duplicate MP4 track IDs should be rejected")
	}
	badRot := DefaultOptions()
	badRot.Rotation = 45
	if _, err := BuildMP4(badRot); err == nil {
		t.Fatal("invalid rotation should be rejected")
	}
	badMxf := DefaultMxfOptions()
	badMxf.AudioEditRate = 0
	if _, err := BuildMXF(badMxf); err == nil {
		t.Fatal("zero MXF audio rate should be rejected")
	}
}
