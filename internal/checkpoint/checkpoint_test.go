package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	want := State{Committed: 128, FrameSums: []string{"aa", "bb"}}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(dir)
	if err != nil || !ok {
		t.Fatalf("Load = %+v, %v, %v", got, ok, err)
	}
	if got.Committed != want.Committed || len(got.FrameSums) != 2 ||
		got.FrameSums[0] != "aa" || got.FrameSums[1] != "bb" {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadMissing(t *testing.T) {
	_, ok, err := Load(t.TempDir())
	if err != nil || ok {
		t.Fatalf("Load = %v, %v; want false, nil", ok, err)
	}
}

func TestLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir); err == nil {
		t.Fatal("expected error for corrupt checkpoint")
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, State{Committed: 64}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName+".tmp")); !os.IsNotExist(err) {
		t.Fatal("temp file left behind after save")
	}
	// Overwrite keeps the latest state.
	if err := Save(dir, State{Committed: 128}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(dir)
	if err != nil || !ok || got.Committed != 128 {
		t.Fatalf("got %+v, %v, %v", got, ok, err)
	}
}

func TestLoadNegativeCommitted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName),
		[]byte(`{"committed":-1,"frame_sums":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir); err == nil {
		t.Fatal("expected error for negative committed count")
	}
}
