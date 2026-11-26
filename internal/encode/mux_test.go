package encode_test

import (
	"os"
	"testing"

	"mediapipe/internal/encode"
	"mediapipe/internal/media"
	"mediapipe/internal/source"
)

func TestRemuxSegmentAtomicAndPadBudget(t *testing.T) {
	dir := t.TempDir()
	src := dir + "/source.mp4"
	err := source.GenerateSyntheticMP4(source.SyntheticConfig{
		Path: src, Seconds: 2, Width: 160, Height: 90, FPS: 10, BitrateKbps: 300, Seed: 17,
	})
	if err != nil {
		t.Fatal(err)
	}
	dst := dir + "/segment.mp4"
	const target = int64(120_000)
	n, sum, err := encode.RemuxSegmentAtomic(src, dst, 0, 1, target)
	if err != nil {
		t.Fatal(err)
	}
	if n != target {
		t.Fatalf("padded size = %d, want %d", n, target)
	}
	got, err := media.FileSHA256(dst)
	if err != nil || got != sum {
		t.Fatalf("hash mismatch got=%q sum=%q err=%v", got, sum, err)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary mux file was not renamed/cleaned")
	}
	info, err := source.Probe(dst)
	if err != nil {
		t.Fatalf("padded segment is not probeable: %v", err)
	}
	if !info.Boxes.MoovComplete {
		t.Fatal("segment moov incomplete")
	}
}
