package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/media"
)

func TestCheckpointRoundTripAndRollback(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.bin")
	cpPath := filepath.Join(dir, "cp.json")
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Two segments, 16 bytes each.
	if _, err := out.Write(make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	m := NewManager(cpPath)
	m.Init("fp", "map", outPath, 4)
	ts := []TrackState{{Key: "mp4:1", OutputID: 1, Written: 1, SHA256: "aa"}}
	if err := m.AppendSegment(out, 0, 16, 1, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write(make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendSegment(out, 16, 32, 2, []TrackState{{Key: "mp4:1", OutputID: 1, Written: 2, SHA256: "bb"}}); err != nil {
		t.Fatal(err)
	}

	plan, err := Load(cpPath, "fp", "map", outPath)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Offset != 32 || plan.Wave != 2 || plan.Written["mp4:1"] != 2 {
		t.Fatalf("plan=%+v", plan)
	}

	// Corrupt the last segment: resume must fall back to the first.
	f, err := os.OpenFile(outPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff}, 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	plan, err = Load(cpPath, "fp", "map", outPath)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Offset != 16 || plan.RolledBack != 1 {
		t.Fatalf("plan=%+v", plan)
	}
	info, _ := os.Stat(outPath)
	if info.Size() != 16 {
		t.Fatalf("output not rolled back: %d", info.Size())
	}
}

func TestCheckpointRejectsWrongInput(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.bin")
	cpPath := filepath.Join(dir, "cp.json")
	if err := os.WriteFile(outPath, make([]byte, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(cpPath)
	m.Init("fp", "map", outPath, 1)
	if err := m.Complete(map[string]string{"mp4:1": "aa"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cpPath, "other", "map", outPath); media.ClassOf(err) != media.ClassCheckpointCorrupt {
		t.Fatalf("class=%s", media.ClassOf(err))
	}
}
