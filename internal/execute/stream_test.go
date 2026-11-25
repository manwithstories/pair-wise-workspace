package execute_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"archivectl/internal/execute"
	"archivectl/internal/scan"
)

func writeFragment(t *testing.T, dir, name string, base uint64, v1 bool) string {
	t.Helper()
	b := scan.BuildFragment(scan.FragmentSpec{
		TrackID: 1, Sequence: 1, BaseDecodeTime: base, TFDTv1: v1,
		Samples: []scan.SampleSpec{
			{Duration: 3000, Size: 16, CTO: 6000},
			{Duration: 3000, Size: 20, CTO: 3000},
			{Duration: 3000, Size: 12, CTO: 0},
		},
	})
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func scanned(t *testing.T, p string) *scan.Fragment {
	t.Helper()
	rep, err := scan.ScanFile(p, "v")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Record.HasFatal() {
		t.Fatalf("scan faults: %v", rep.Record.Faults)
	}
	return rep.Fragments[0]
}

func TestRewritePatchesTFDTAndPreservesPayload(t *testing.T) {
	dir := t.TempDir()
	src := writeFragment(t, dir, "seg0.m4s", 9_000_000, true)
	orig, _ := os.ReadFile(src)
	frag := scanned(t, src)

	patch, err := execute.TFDTYPatch(frag.TFDT, 42)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out", "seg0.repaired.m4s")
	size, sha, err := execute.RewriteFileAtomic(src, dst, []execute.Patch{patch})
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(orig)) || sha == "" {
		t.Fatalf("size=%d orig=%d sha=%q", size, len(orig), sha)
	}

	out, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(orig) {
		t.Fatalf("rewrite changed length %d -> %d", len(orig), len(out))
	}
	// Every byte except the same-width tfdt value must be identical.
	vo := frag.TFDT.ValueOffset
	w := frag.TFDT.ValueWidth
	for i := range orig {
		inPatch := int64(i) >= vo && int64(i) < vo+int64(w)
		if !inPatch && out[i] != orig[i] {
			t.Fatalf("non-patch byte %d changed", i)
		}
	}
	// Media payload ranges must match exactly.
	for _, s := range frag.Samples {
		start := s.PayloadOffset
		if !bytes.Equal(orig[start:start+int64(s.Size)], out[start:start+int64(s.Size)]) {
			t.Fatalf("sample %d payload altered", s.Index)
		}
	}
	repaired := scanned(t, dst)
	if repaired.TFDT.Base != 42 {
		t.Fatalf("repaired base=%d want 42", repaired.TFDT.Base)
	}
	if len(repaired.Samples) != 3 || repaired.Samples[1].Size != 20 {
		t.Fatal("sample table not preserved after patch")
	}
}

func TestRewriteV0PatchFitsAndResumeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	src := writeFragment(t, dir, "v0.m4s", 0xFFFFFF00, false)
	frag := scanned(t, src)
	patch, err := execute.TFDTYPatch(frag.TFDT, 1234) // rebased small value fits uint32
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "o", "v0.out")
	if _, _, err := execute.RewriteFileAtomic(src, dst, []execute.Patch{patch}); err != nil {
		t.Fatal(err)
	}
	sha1, _ := readSHA(t, dst)
	if !execute.ChunkComplete(dst, int64(len(mustRead(t, src)))) {
		t.Fatal("chunk should be complete after atomic write")
	}
	// Re-running overwrites atomically with identical content.
	if _, _, err := execute.RewriteFileAtomic(src, dst, []execute.Patch{patch}); err != nil {
		t.Fatal(err)
	}
	sha2, _ := readSHA(t, dst)
	if sha1 != sha2 {
		t.Fatal("repeated rewrite changed output digest")
	}
	repaired := scanned(t, dst)
	if repaired.TFDT.Base != 1234 || repaired.TFDT.Version != 0 {
		t.Fatalf("v0 patch wrong: %+v", repaired.TFDT)
	}
}

func TestCleanStaleTemp(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.m4s.tmp", "b.json.tmp-12345", "keep.m4s"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := execute.CleanStaleTemp(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.m4s")); err != nil {
		t.Fatal("final chunk must be kept")
	}
	for _, n := range []string{"a.m4s.tmp", "b.json.tmp-12345"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Fatalf("stale temp %s not removed", n)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readSHA(t *testing.T, p string) (string, error) {
	t.Helper()
	b := mustRead(t, p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
