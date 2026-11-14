package mp4atom_test

import (
	"path/filepath"
	"testing"

	"mp4replay/internal/fixture"
	"mp4replay/internal/mp4atom"
	"mp4replay/internal/report"
	"mp4replay/internal/seekidx"
)

func openFixture(t *testing.T, opt fixture.Options) *mp4atom.Movie {
	t.Helper()
	opt.Path = filepath.Join(t.TempDir(), "f.mp4")
	if err := fixture.Generate(opt); err != nil {
		t.Fatal(err)
	}
	m, err := mp4atom.Open(opt.Path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCo64Offsets(t *testing.T) {
	m := openFixture(t, fixture.Options{
		Tracks: 1, Frames: 50, UseCo64: true, Variant: fixture.VariantPlain})
	defer m.Close()
	tr := m.VideoTrack()
	// every offset must be positive, increasing and within a plausible range
	prev := int64(-1)
	for _, s := range tr.Samples {
		if s.Offset <= prev {
			t.Fatalf("offsets not increasing: %d after %d", s.Offset, prev)
		}
		if s.Size == 0 {
			t.Fatal("zero sample size")
		}
		prev = s.Offset
	}
}

func TestCttsV1AndWrap(t *testing.T) {
	m := openFixture(t, fixture.Options{
		Tracks: 2, Frames: 1200, Variant: fixture.VariantElstCttsWrap})
	defer m.Close()
	tr := m.VideoTrack()
	if tr.Table.CttsVer != 1 {
		t.Fatalf("want ctts v1, got %d", tr.Table.CttsVer)
	}
	if !tr.Table.HasCtts {
		t.Fatal("expected ctts present")
	}
	if tr.Timeline.WrapEvents == 0 {
		t.Fatal("expected uint32 wrap events on long dts")
	}
	if tr.Timeline.EmptyEdits != 1 {
		t.Fatalf("want 1 empty edit, got %d", tr.Timeline.EmptyEdits)
	}
}

func TestSeekBounds(t *testing.T) {
	m := openFixture(t, fixture.Options{
		Tracks: 1, Frames: 100, Variant: fixture.VariantPlain})
	defer m.Close()
	idx := seekidx.Build(m.VideoTrack())
	if _, err := idx.Locate(-1); err == nil {
		t.Fatal("expected error before first frame")
	} else if e, ok := err.(*report.Error); !ok || e.Code != report.ExitSeekOOB {
		t.Fatalf("want seek OOB code, got %v", err)
	}
	last := idx.PtsMs[len(idx.PtsMs)-1]
	if _, err := idx.Locate(last + 100000); err == nil {
		t.Fatal("expected error after last frame")
	}
}

func TestSeekLandsOnSync(t *testing.T) {
	m := openFixture(t, fixture.Options{
		Tracks: 1, Frames: 200, Variant: fixture.VariantPlain})
	defer m.Close()
	tr := m.VideoTrack()
	idx := seekidx.Build(tr)
	mid := idx.PtsMs[len(idx.PtsMs)/2]
	land, err := idx.Locate(mid)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Samples[land.Key.SampleIndex].Sync {
		t.Fatal("landing sample is not a sync frame")
	}
	if land.Key.PtsMs > mid {
		t.Fatal("landing is after requested time")
	}
}
