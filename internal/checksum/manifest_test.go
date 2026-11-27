package checksum

import (
	"os"
	"path/filepath"
	"testing"
)

const manifestName = "manifest.json"

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b/b.dat", "second")
	writeFile(t, dir, "a/a.dat", "first")
	m1, err := Build(dir, manifestName)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted regardless of filesystem walk order.
	if m1.Files[0].Path != "a/a.dat" || m1.Files[1].Path != "b/b.dat" {
		t.Fatalf("manifest not sorted: %+v", m1.Files)
	}
	if len(m1.ManifestDigest) != 64 {
		t.Fatalf("manifest digest = %q", m1.ManifestDigest)
	}
	// Rebuild over identical content yields the identical digest.
	m2, _ := Build(dir, manifestName)
	if m2.ManifestDigest != m1.ManifestDigest {
		t.Fatal("rebuild must produce the same manifest digest")
	}
	if m1.Files[0].SHA256 != HashBytes([]byte("first")) {
		t.Fatal("per-file digest mismatch")
	}
}

func TestWriteAndVerify(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "frag.mp4", "media-bytes")
	m, err := Build(dir, manifestName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(dir, manifestName, m); err != nil {
		t.Fatal(err)
	}
	if err := Verify(dir, manifestName); err != nil {
		t.Fatalf("fresh manifest must verify: %v", err)
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "frag.mp4", "original")
	m, _ := Build(dir, manifestName)
	_, _ = Write(dir, manifestName, m)

	// Tamper with the artifact after the manifest was sealed.
	writeFile(t, dir, "frag.mp4", "tampered")
	if err := Verify(dir, manifestName); err == nil {
		t.Fatal("tampered artifact must fail verification")
	}

	// Restore content; verification passes again.
	writeFile(t, dir, "frag.mp4", "original")
	if err := Verify(dir, manifestName); err != nil {
		t.Fatalf("restored artifact must verify: %v", err)
	}
}

func TestVerifyDetectsExtraFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.dat", "x")
	m, _ := Build(dir, manifestName)
	_, _ = Write(dir, manifestName, m)
	writeFile(t, dir, "extra.dat", "y")
	if err := Verify(dir, manifestName); err == nil {
		t.Fatal("unexpected extra file must fail verification")
	}
}

func TestManifestExcludesItself(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.dat", "x")
	m, _ := Build(dir, manifestName)
	path, _ := Write(dir, manifestName, m)
	for _, f := range m.Files {
		if f.Path == manifestName {
			t.Fatal("manifest must not hash itself")
		}
	}
	// Building again with the manifest present still gives the same set.
	m2, err := Build(dir, manifestName)
	if err != nil || len(m2.Files) != 1 || m2.ManifestDigest != m.ManifestDigest {
		t.Fatalf("manifest presence changed digest: %+v err=%v path=%s", m2, err, path)
	}
}
