package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadAndValidateInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "checkpoint.json")
	cp := &Checkpoint{Seed: 17, Rungs: 3, Segments: 24, InputSHA256: "abc"}
	cp.SetStage("transport", "digest", true)
	if err := Save(path, cp); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != Schema || got.Seed != 17 {
		t.Fatalf("checkpoint=%+v", got)
	}
	if err := got.ValidateInput("abc"); err != nil {
		t.Fatal(err)
	}
	if err := got.ValidateInput("different"); err == nil {
		t.Fatal("input digest mismatch accepted")
	}
	if _, ok := got.Stage("transport"); !ok {
		t.Fatal("stage missing")
	}
}

func TestVerifyArtifactsDetectsTampering(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "master.m3u8")
	if err := os.WriteFile(file, []byte("#EXTM3U\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	sum, size, err := HashFile(file)
	if err != nil {
		t.Fatal(err)
	}
	art := Artifact{Path: "master.m3u8", SHA256: sum, Size: size}
	agg, err := HashArtifactSet([]Artifact{art})
	if err != nil {
		t.Fatal(err)
	}
	cp := &Checkpoint{Artifacts: []Artifact{art}, OutputSHA256: agg, Completed: true}
	if _, err := cp.VerifyArtifacts(dir); err != nil {
		t.Fatalf("valid artifacts: %v", err)
	}
	if err := os.WriteFile(file, []byte("#tampered\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.VerifyArtifacts(dir); err == nil {
		t.Fatal("tampered artifact accepted")
	}
}

func TestVerifyRejectsUnsafeArtifactPath(t *testing.T) {
	cp := &Checkpoint{Artifacts: []Artifact{{Path: "../escape"}}}
	if _, err := cp.VerifyArtifacts(t.TempDir()); err == nil {
		t.Fatal("path traversal artifact accepted")
	}
}
