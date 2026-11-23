package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundtrip(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "cp.json")}
	if _, ok, err := s.Load(); err != nil || ok {
		t.Fatalf("empty load: ok=%v err=%v", ok, err)
	}
	want := Checkpoint{Stage: "parse", InputDigest: "in", OutputDigest: "out", Source: "src"}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.Stage != want.Stage || got.InputDigest != want.InputDigest ||
		got.OutputDigest != want.OutputDigest || got.Source != want.Source {
		t.Fatalf("got %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt not set")
	}
}

func TestSaveFailureKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "cp.json")}
	if err := s.Save(Checkpoint{Stage: "parse"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if err := s.Save(Checkpoint{Stage: "transport"}); err == nil {
		t.Fatal("want save error")
	}
	got, ok, err := s.Load()
	if err != nil || !ok || got.Stage != "parse" {
		t.Fatalf("previous checkpoint damaged: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestCorruptCheckpoint(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "cp.json")}
	if err := os.WriteFile(s.Path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load(); err == nil {
		t.Fatal("want error for corrupt checkpoint")
	}
}
