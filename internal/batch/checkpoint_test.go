package batch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cp := &Checkpoint{Version: 1, InputFingerprint: "fp", ConfigDigest: "dg", TotalBlocks: 2}
	cp.Add(BlockState{Cam: 1, Index: 0, Frames: 10, SHA256: "b"})
	cp.Add(BlockState{Cam: 0, Index: 0, Frames: 10, SHA256: "a"})
	if err := cp.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	back, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back.InputFingerprint != "fp" || len(back.Done) != 2 {
		t.Fatalf("bad checkpoint: %+v", back)
	}
	// Done is sorted by (cam, index) for a deterministic file.
	if back.Done[0].Cam != 0 || back.Done[1].Cam != 1 {
		t.Fatalf("unsorted: %+v", back.Done)
	}
	if !back.Has(1, 0) || back.Has(0, 1) {
		t.Fatal("Has mismatch")
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	cp, err := Load(dir)
	if err != nil || cp != nil {
		t.Fatalf("missing checkpoint: %v %v", cp, err)
	}
	os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte("{broken"), 0o644)
	if _, err := Load(dir); err == nil {
		t.Fatal("corrupt checkpoint must error")
	}
}

func TestFingerprintFiles(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.bin")
	b := filepath.Join(dir, "b.bin")
	os.WriteFile(a, []byte("aaa"), 0o644)
	os.WriteFile(b, []byte("bbb"), 0o644)
	fp1, err := FingerprintFiles([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	fp2, _ := FingerprintFiles([]string{b, a}) // order-independent
	if fp1 != fp2 {
		t.Fatal("fingerprint depends on order")
	}
	os.WriteFile(a, []byte("changed"), 0o644)
	fp3, _ := FingerprintFiles([]string{a, b})
	if fp3 == fp1 {
		t.Fatal("fingerprint ignores content change")
	}
}

func TestDigestConfig(t *testing.T) {
	d1, _ := DigestConfig(struct{ A int }{1})
	d2, _ := DigestConfig(struct{ A int }{2})
	if d1 == d2 || len(d1) != 64 {
		t.Fatal("bad config digest")
	}
}
