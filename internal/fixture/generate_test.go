package fixture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"turncode/internal/config"
)

func TestGenerateDeterministicAndConsistent(t *testing.T) {
	opts := Options{Width: 32, Height: 18, FPS: 10, Frames: 24, Scene: "mixed", Seed: 77, OutDir: t.TempDir()}
	first, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(opts.OutDir, first.YUVFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != first.FileBytes {
		t.Fatalf("file size %d want %d", info.Size(), first.FileBytes)
	}
	raw, err := os.ReadFile(filepath.Join(opts.OutDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded config.Spec
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SHA256 != first.SHA256 {
		t.Fatal("manifest hash mismatch")
	}
	opts.OutDir = t.TempDir()
	second, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.SHA256 != first.SHA256 {
		t.Fatal("same seed produced different media")
	}
	opts.Seed++
	third, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	if third.SHA256 == first.SHA256 {
		t.Fatal("different seed produced identical media")
	}
}

func TestGenerateRejectsOddDimensions(t *testing.T) {
	_, err := Generate(Options{Width: 31, Height: 18, FPS: 10, Frames: 2, Scene: "slow", OutDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected odd width rejection")
	}
}
