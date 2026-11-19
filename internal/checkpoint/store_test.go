package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadValidate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cp.json")
	cp := &Checkpoint{
		InputSHA256: "aaa", ConfigSHA256: "bbb",
		RecordsProcessed: 5000, OutputCommitted: 12345,
		Pipeline: []byte(`{"x":1}`),
	}
	if err := Save(p, cp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file should be renamed away")
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.RecordsProcessed != 5000 || got.OutputCommitted != 12345 {
		t.Fatalf("%+v", got)
	}
	if err := got.Validate("aaa", "bbb"); err != nil {
		t.Fatal(err)
	}
	if err := got.Validate("ccc", "bbb"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want digest mismatch, got %v", err)
	}
	if err := got.Validate("aaa", "ccc"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want digest mismatch, got %v", err)
	}
}

func TestLoadMissing(t *testing.T) {
	cp, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || cp != nil {
		t.Fatalf("cp=%v err=%v", cp, err)
	}
}

func TestLoadCorrupt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cp.json")
	if err := CorruptFile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected corrupt checkpoint error")
	}
}
