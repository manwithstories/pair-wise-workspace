package align

import (
	"testing"

	"segmanifest/internal/ladder"
	"segmanifest/internal/manifest"
)

func alignedManifest(offsetUS int64, starts ...int64) *manifest.Manifest {
	m := &manifest.Manifest{TargetUS: 4_000_000, ToleranceUS: 2_000, Template: manifest.DefaultTemplate}
	rep := manifest.Representation{ID: "r1080", Bandwidth: 6_000_000, AudioOffsetUS: offsetUS}
	for i, start := range starts {
		rep.Segments = append(rep.Segments, manifest.Segment{Number: uint64(i + 1), StartUS: start, DurationUS: 4_000_000})
	}
	m.Representations = []manifest.Representation{rep}
	return m
}

func TestAlignSnapsAndMeasuresDrift(t *testing.T) {
	m := alignedManifest(0, 1_000, 4_001_000, 7_999_000)
	grid, err := ladder.BuildGrid(m.Representations, 4_000_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := Align(m, grid); err != nil {
		t.Fatal(err)
	}
	for i, wantStart := range []int64{0, 4_000_000, 8_000_000} {
		if m.Representations[0].Segments[i].StartUS != wantStart {
			t.Fatalf("segment %d start=%d want %d", i+1, m.Representations[0].Segments[i].StartUS, wantStart)
		}
	}
	if got := m.Stats[0].MaxDriftUS; got != 1_000 {
		t.Fatalf("max drift=%d", got)
	}
	if len(m.Discontinuities) != 0 {
		t.Fatalf("unexpected discontinuities: %+v", m.Discontinuities)
	}
}

func TestAlignResyncAfterThreeSameDirectionOverruns(t *testing.T) {
	m := alignedManifest(0, 3_000, 4_003_000, 8_003_000)
	grid, err := ladder.BuildGrid(m.Representations, 4_000_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := Align(m, grid); err != nil {
		t.Fatal(err)
	}
	if len(m.Discontinuities) != 3 || len(m.ResyncPoints) != 1 {
		t.Fatalf("disc=%d resync=%d", len(m.Discontinuities), len(m.ResyncPoints))
	}
	if m.ResyncPoints[0].SegmentNumber != 3 || m.ResyncPoints[0].Direction != 1 {
		t.Fatalf("resync=%+v", m.ResyncPoints[0])
	}
	for i := 1; i < len(m.Representations[0].Segments); i++ {
		if m.Representations[0].Segments[i].Number <= m.Representations[0].Segments[i-1].Number {
			t.Fatal("segment numbers rolled back")
		}
	}
}

func TestAlignNegativeAudioOffsetTruncatesFirstSegment(t *testing.T) {
	m := alignedManifest(-37_000, 0, 4_000_000)
	grid, err := ladder.BuildGrid(m.Representations, 4_000_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := Align(m, grid); err != nil {
		t.Fatal(err)
	}
	if !m.Representations[0].Segments[0].Truncated {
		t.Fatal("negative audio offset did not truncate the first segment")
	}
}

func TestAlignNegativeRawStartTruncatesFirstSegment(t *testing.T) {
	m := alignedManifest(0, -10_000, 3_990_000)
	grid, err := ladder.BuildGrid(m.Representations, 4_000_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := Align(m, grid); err != nil {
		t.Fatal(err)
	}
	if !m.Representations[0].Segments[0].Truncated || m.Representations[0].Segments[0].StartUS != 0 {
		t.Fatalf("segment=%+v", m.Representations[0].Segments[0])
	}
}
