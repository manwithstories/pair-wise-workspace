package pipe_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archivecheck/internal/fault"
	"archivecheck/internal/pipe"
	"archivecheck/internal/testkit"
)

func writeManifest(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, "manifest.txt")
	content := "# test manifest\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadManifestRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, "init ../outside.mp4")
	_, err := pipe.LoadManifest(dir, path)
	if !fault.IsKind(err, fault.KindPath) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadManifestRejectsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, "init /etc/passwd")
	_, err := pipe.LoadManifest(dir, path)
	if !fault.IsKind(err, fault.KindPath) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadManifestRejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeManifest(t, dir, "init sub")
	_, err := pipe.LoadManifest(dir, path)
	if !fault.IsKind(err, fault.KindNotRegular) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadManifestRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.mp4"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real.mp4"), filepath.Join(dir, "link.mp4")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	path := writeManifest(t, dir, "init link.mp4")
	_, err := pipe.LoadManifest(dir, path)
	if !fault.IsKind(err, fault.KindNotRegular) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadManifestMultipleGroups(t *testing.T) {
	layout, err := testkit.WriteDemoInputs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(layout.ManifestPath)
	path := writeManifest(t, dir,
		"init init.mp4",
		"media frag-001.m4s",
		"media frag-002.m4s",
		"init init-2.mp4",
		"media frag-003.m4s",
	)
	// The second init is a copy of the first; groups are independent.
	data, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "init-2.mp4"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := pipe.LoadManifest(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(m.Groups))
	}
	if len(m.Groups[0].Media) != 2 || len(m.Groups[1].Media) != 1 {
		t.Fatalf("group media = %d/%d", len(m.Groups[0].Media), len(m.Groups[1].Media))
	}
	if len(m.Digest) != 64 {
		t.Fatalf("digest = %q", m.Digest)
	}
	// A restored program maps to one init layout; the pipeline rejects mixed
	// groups rather than silently interleaving incompatible tracks.
	if _, err := pipe.LoadInputs(m); !fault.IsKind(err, fault.KindPath) {
		t.Fatalf("LoadInputs err = %v, want unsafe-path fault", err)
	}
}

func TestManifestDigestChangesWithContent(t *testing.T) {
	layout, err := testkit.WriteDemoInputs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(layout.ManifestPath)
	m1, err := pipe.LoadManifest(dir, layout.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(dir, "frag-001.m4s")
	data, _ := os.ReadFile(media)
	if err := os.WriteFile(media, append(data, 0x00), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, err := pipe.LoadManifest(dir, layout.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Digest == m2.Digest {
		t.Fatal("digest did not change with input content")
	}
}
