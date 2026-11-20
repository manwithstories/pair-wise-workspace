package fixture

import (
	"testing"

	"mediarepair/internal/parse"
)

func TestGenerateSmoke(t *testing.T) {
	dir := t.TempDir()
	files, err := Generate(dir, DefaultSpec())
	if err != nil {
		t.Fatal(err)
	}
	mf, err := parse.OpenMP4(files.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tracks=%v samples=%d", mf.Tracks(), mf.TotalSamples())
	if mf.TotalSamples() != 180 {
		t.Fatalf("samples=%d", mf.TotalSamples())
	}
	tl, err := parse.LoadTimeline(files.Sidecar)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("clips=%d transitions=%d", len(tl.Clips), len(tl.Transitions))
	if _, err := parse.OpenMP4(files.Corrupt); err == nil {
		t.Fatal("corrupt 应当解析失败")
	}
}
