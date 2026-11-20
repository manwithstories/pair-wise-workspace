package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New("digest-1", map[string]string{"seed": "42"})
	if err := s.MarkDone("step-a", map[string]int{"n": 3}); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("tmp file should not remain")
	}
	loaded, err := Load(dir)
	if err != nil || loaded == nil {
		t.Fatalf("load=%v err=%v", loaded, err)
	}
	if !loaded.IsDone("step-a") || loaded.IsDone("step-b") {
		t.Fatalf("completed=%v", loaded.Completed)
	}
	var res map[string]int
	if !loaded.Get("step-a", &res) || res["n"] != 3 {
		t.Fatalf("res=%v", res)
	}
	if err := loaded.Validate("digest-1", map[string]string{"seed": "42"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsChanges(t *testing.T) {
	s := New("digest-1", map[string]string{"seed": "42"})
	if err := s.Validate("digest-2", map[string]string{"seed": "42"}); !errors.Is(err, ErrInputChanged) {
		t.Fatalf("err=%v", err)
	}
	if err := s.Validate("digest-1", map[string]string{"seed": "43"}); !errors.Is(err, ErrInputChanged) {
		t.Fatalf("err=%v", err)
	}
	s.Version = "old"
	if err := s.Validate("digest-1", map[string]string{"seed": "42"}); !errors.Is(err, ErrVersionChanged) {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadMissing(t *testing.T) {
	s, err := Load(t.TempDir())
	if err != nil || s != nil {
		t.Fatalf("s=%v err=%v", s, err)
	}
}

func TestDigestInputsDeterministic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.bin"), []byte("2"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.bin"), []byte("1"), 0o644)
	d1, err := DigestInputs(dir, []string{"a.bin", "b.bin"})
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := DigestInputs(dir, []string{"b.bin", "a.bin"})
	if d1 != d2 {
		t.Fatal("order must not matter")
	}
	os.WriteFile(filepath.Join(dir, "a.bin"), []byte("changed"), 0o644)
	d3, _ := DigestInputs(dir, []string{"a.bin", "b.bin"})
	if d3 == d1 {
		t.Fatal("content change must change digest")
	}
}
