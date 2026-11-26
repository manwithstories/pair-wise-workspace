package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediad/internal/media"
)

func TestParseManifestRejectsBadTimebase(t *testing.T) {
	in := `{"camera":"c","file":"a/b.mov","track":"t","timebase":"0/48000","wrap_bits":33,"anchors":[{"host_us":1,"media_ticks":0}]}`
	_, err := ParseManifest(strings.NewReader(in))
	me, ok := media.AsMediaError(err)
	if !ok || me.Code != media.ErrInvalidTimebase {
		t.Fatalf("code=%v err=%v", me, err)
	}
}

func TestParseManifestRejectsDuplicateTrack(t *testing.T) {
	in := strings.Join([]string{
		`{"camera":"c","file":"a.mov","track":"t","timebase":"1/48000","wrap_bits":33,"anchors":[{"host_us":1,"media_ticks":0}]}`,
		`{"camera":"c","file":"a.mov","track":"t","timebase":"1/48000","wrap_bits":33,"anchors":[{"host_us":2,"media_ticks":0}]}`,
	}, "\n")
	_, err := ParseManifest(strings.NewReader(in))
	if err == nil {
		t.Fatal("duplicate track accepted")
	}
}

func TestParseManifestRejectsBadWrapBits(t *testing.T) {
	in := `{"camera":"c","file":"a.mov","track":"t","timebase":"1/48000","wrap_bits":4,"anchors":[{"host_us":1,"media_ticks":0}]}`
	if _, err := ParseManifest(strings.NewReader(in)); err == nil {
		t.Fatal("bad wrap width accepted")
	}
}

func TestParseEditsRejectsNegativeDuration(t *testing.T) {
	in := `{"camera":"c","track":"t","in":100,"out":100,"seq":0}`
	_, err := ParseEdits(strings.NewReader(in))
	if err == nil {
		t.Fatal("zero duration accepted")
	}
}

func TestParseEditsFractionalTickRejected(t *testing.T) {
	in := `{"camera":"c","track":"t","in":"1/3","out":100,"seq":0}`
	if _, err := ParseEdits(strings.NewReader(in)); err == nil {
		t.Fatal("fractional tick accepted")
	}
}

func TestResolveWithinRejectsEscape(t *testing.T) {
	root := t.TempDir()
	mediaDir := filepath.Join(root, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "ok.mov"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Direct traversal.
	if _, err := ResolveWithin(root, "../etc/passwd.mov"); err == nil {
		t.Fatal("traversal accepted")
	}
	// Absolute path.
	if _, err := ResolveWithin(root, "/etc/passwd.mov"); err == nil {
		t.Fatal("absolute path accepted")
	}
	// Symlink escape.
	outside := filepath.Join(t.TempDir(), "secret.mov")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mediaDir, "link.mov")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ResolveWithin(root, "media/link.mov"); err == nil {
		t.Fatal("symlink escape accepted")
	}
	// A legitimate path resolves.
	if got, err := ResolveWithin(root, "media/ok.mov"); err != nil {
		t.Fatalf("legit path rejected: %v", err)
	} else if got == "" {
		t.Fatal("empty resolution")
	}
}

func TestResolveWithinRejectsForeignExtension(t *testing.T) {
	root := t.TempDir()
	if _, err := ResolveWithin(root, "media/clip.wav"); err == nil {
		t.Fatal("non-MOV/MP4 accepted")
	}
}
