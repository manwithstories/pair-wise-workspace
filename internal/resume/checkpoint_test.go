package resume

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/mediaerr"
)

func TestCheckpointPrefixTamperAndOutputHash(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "r0.ts")
	if err := os.WriteFile(input, []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp := New(73021, dir, 128)
	if err := cp.RecordInputPrefix("r0", input, 8, 1, 90000); err != nil {
		t.Fatal(err)
	}
	cpPath := filepath.Join(dir, "checkpoint.json")
	if err := cp.Save(cpPath); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.VerifyPrefixes(); err != nil {
		t.Fatal(err)
	}

	// Mutating bytes after the protected prefix is allowed.
	f, err := os.OpenFile(input, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("XX"), 12); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := loaded.VerifyPrefixes(); err != nil {
		t.Fatalf("suffix mutation should not affect prefix: %v", err)
	}

	// Mutating the prefix must fail with the structured mismatch kind.
	if err := os.WriteFile(input, []byte("0123X56789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = loaded.VerifyPrefixes()
	var se *mediaerr.Error
	if !errors.As(err, &se) || se.Kind != mediaerr.KindPrefixMismatch {
		t.Fatalf("err=%v want prefix mismatch", err)
	}
}

func TestCommitOutputIdempotentAndDetectsTampering(t *testing.T) {
	dir := t.TempDir()
	cp := New(1, dir, 0)
	data := []byte("deterministic CMAF")
	replayed, err := cp.CommitOutput(dir, "v0/0", filepath.Join("v0", "seg_0000.m4s"), data)
	if err != nil || replayed {
		t.Fatalf("first commit replayed=%v err=%v", replayed, err)
	}
	replayed, err = cp.CommitOutput(dir, "v0/0", filepath.Join("v0", "seg_0000.m4s"), data)
	if err != nil || !replayed {
		t.Fatalf("identical recommit should be replayed/skipped: replayed=%v err=%v", replayed, err)
	}
	if err := cp.VerifyOutputs(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "v0", "seg_0000.m4s"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cp.VerifyOutputs(dir); err == nil {
		t.Fatal("expected output hash verification failure")
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	cp, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || cp != nil {
		t.Fatalf("cp=%v err=%v", cp, err)
	}
}
