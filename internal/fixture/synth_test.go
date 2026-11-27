package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"avpipe/internal/media"
)

func TestGenerateAllIsDeterministic(t *testing.T) {
	if testing.Short() {
		t.Skip("fixture generation shells out to ffmpeg")
	}
	a, err := os.MkdirTemp("", "avpipe-det-a-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(a)
	b, err := os.MkdirTemp("", "avpipe-det-b-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(b)
	setA, err := GenerateAll(a, 17)
	if err != nil {
		t.Fatal(err)
	}
	setB, err := GenerateAll(b, 17)
	if err != nil {
		t.Fatal(err)
	}
	if len(setA.Files) != len(setB.Files) {
		t.Fatalf("file count mismatch %d vs %d", len(setA.Files), len(setB.Files))
	}
	for i := range setA.Files {
		na := filepath.Base(setA.Files[i].Path)
		nb := filepath.Base(setB.Files[i].Path)
		if na != nb {
			t.Fatalf("name mismatch %s vs %s", na, nb)
		}
		if setA.Files[i].SHA256 != setB.Files[i].SHA256 {
			t.Fatalf("%s not deterministic: %s vs %s", na, setA.Files[i].SHA256, setB.Files[i].SHA256)
		}
	}
}

func TestEnsureSafePathRejectsEscapes(t *testing.T) {
	g, err := New(t.TempDir())
	if err != nil {
		t.Skipf("fixtures need ffmpeg: %v", err)
	}
	for _, bad := range []string{"../escape.mp4", "a/../../escape.mp4", "/etc/passwd", ""} {
		if _, err := g.EnsureSafePath(bad); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
	if _, err := g.EnsureSafePath("nested/dir/file.mp4"); err != nil {
		t.Fatalf("nested safe path rejected: %v", err)
	}
}

func TestTruncateRejectsBadKeep(t *testing.T) {
	g, err := New(t.TempDir())
	if err != nil {
		t.Skipf("fixtures need ffmpeg: %v", err)
	}
	f, err := g.Generate(Spec{Kind: KindGood, Name: "x.mp4", Format: "mp4", Video: true, Seconds: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Truncate("x.mp4", "y.mp4", 0); media.KindOf(err) != media.KindUsage {
		t.Fatalf("keep=0 kind = %v", media.KindOf(err))
	}
	if _, err := g.Truncate("x.mp4", "y.mp4", f.Size); media.KindOf(err) != media.KindUsage {
		t.Fatalf("keep=size kind = %v", media.KindOf(err))
	}
}

func TestHashFileStable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.bin")
	if err := os.WriteFile(p, []byte("avpipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || len(a) != 64 {
		t.Fatalf("hash %q vs %q", a, b)
	}
}
