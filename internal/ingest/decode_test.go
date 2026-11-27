package ingest

import (
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"fieldcut/internal/media"
)

func writePNG(t *testing.T, path string, w, h int) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()
	sum, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestCheckFramesClassifiesCorruptAndFatal(t *testing.T) {
	dir := t.TempDir()
	good := Entry{Track: "A", Seq: 1, Width: 4, Height: 4, Path: "f/a1.png"}
	good.SHA256 = writePNG(t, filepath.Join(dir, good.Path), 4, 4)

	bad := Entry{Track: "A", Seq: 2, Width: 4, Height: 4, Path: "f/a2.png"}
	if err := os.MkdirAll(filepath.Join(dir, "f"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bad.Path), []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad.SHA256, _ = HashFile(filepath.Join(dir, bad.Path))

	mismatch := good
	mismatch.Seq = 3
	mismatch.SHA256 = repeat("0", 64)

	entries := map[string][]Entry{"A": {good, bad}}
	verdicts, err := CheckFrames(dir, entries)
	if err != nil {
		t.Fatalf("CheckFrames fatal: %v", err)
	}
	if verdicts["A"][1] != nil {
		t.Fatalf("good frame flagged: %v", verdicts["A"][1])
	}
	if verdicts["A"][2] == nil || verdicts["A"][2].Kind != media.KindCorrupt {
		t.Fatalf("corrupt not flagged: %v", verdicts["A"][2])
	}
	if _, err := CheckFrames(dir, map[string][]Entry{"A": {mismatch}}); err == nil {
		t.Fatal("hash mismatch was not fatal")
	}
	var me *media.MediaError
	if err := func() error { _, e := CheckFrames(dir, map[string][]Entry{"A": {mismatch}}); return e }(); !errors.As(err, &me) || me.Kind != media.KindCorrupt {
		t.Fatalf("hash mismatch kind = %v", err)
	}
}

func TestCheckFramesDimensionMismatch(t *testing.T) {
	dir := t.TempDir()
	e := Entry{Track: "B", Seq: 0, Width: 9, Height: 9, Path: "b0.png"}
	e.SHA256 = writePNG(t, filepath.Join(dir, e.Path), 4, 4)
	verdicts, err := CheckFrames(dir, map[string][]Entry{"B": {e}})
	if err != nil {
		t.Fatal(err)
	}
	if verdicts["B"][0] == nil {
		t.Fatal("dimension mismatch not flagged")
	}
}
