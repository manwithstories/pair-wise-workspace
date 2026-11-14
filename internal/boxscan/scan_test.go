package boxscan

import (
	"os"
	"path/filepath"
	"testing"

	"mp4replay/internal/fixture"
	"mp4replay/internal/report"
)

func writeFixture(t *testing.T, name string, opts fixture.Options) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	opts.Path = p
	if err := fixture.Generate(opts); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return p
}

func codeOf(err error) int {
	if e, ok := err.(*report.Error); ok {
		return e.Code
	}
	return -1
}

func TestValidFileScans(t *testing.T) {
	p := writeFixture(t, "ok.mp4", fixture.Options{
		Tracks: 2, Frames: 40, Variant: fixture.VariantPlain})
	f, err := Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if FindFirst(f.Roots, "moov") == nil {
		t.Fatal("moov not found")
	}
}

func corrupt(t *testing.T, src string, mutate func([]byte)) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	mutate(b)
	dst := filepath.Join(t.TempDir(), "bad.mp4")
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestTruncatedBox(t *testing.T) {
	src := writeFixture(t, "x.mp4", fixture.Options{Frames: 20})
	// truncate inside the first box header region
	dst := corrupt(t, src, func(b []byte) { /* keep header, cut tail */ })
	_ = dst
	st, _ := os.Stat(src)
	b, _ := os.ReadFile(src)
	short := b[:st.Size()-3]
	p := filepath.Join(t.TempDir(), "trunc.mp4")
	os.WriteFile(p, short, 0o644)
	_, err := Open(p)
	if codeOf(err) != report.ExitBoxInvalid {
		t.Fatalf("want box invalid, got %v", err)
	}
}

func TestSizeSmallerThanHeader(t *testing.T) {
	src := writeFixture(t, "x.mp4", fixture.Options{Frames: 20})
	p := corrupt(t, src, func(b []byte) {
		b[0] = 0
		b[1] = 0
		b[2] = 0
		b[3] = 4 // size 4 < 8
	})
	_, err := Open(p)
	if codeOf(err) != report.ExitBoxInvalid {
		t.Fatalf("want box invalid, got %v", err)
	}
}

func TestSizeBeyondFile(t *testing.T) {
	src := writeFixture(t, "x.mp4", fixture.Options{Frames: 20})
	p := corrupt(t, src, func(b []byte) {
		b[0] = 0x7f
		b[1] = 0xff
		b[2] = 0xff
		b[3] = 0xff
	})
	_, err := Open(p)
	if codeOf(err) != report.ExitBoxInvalid {
		t.Fatalf("want box invalid, got %v", err)
	}
}
