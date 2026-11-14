package fixture

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func acceptanceSpec(t *testing.T) *Spec {
	t.Helper()
	spec, err := Generate(Options{Seed: 7, Representations: 3, Segments: 120, TargetUS: 4_000_000, JitterUS: 1_600, AudioOffsetUS: -37_000})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestGenerateIsDeterministic(t *testing.T) {
	spec := acceptanceSpec(t)
	firstDir, secondDir := t.TempDir(), t.TempDir()
	if err := Write(firstDir, spec); err != nil {
		t.Fatal(err)
	}
	if err := Write(secondDir, spec); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(firstDir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(secondDir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("fixture bytes differ between identical runs")
	}
}

func TestGeneratedTimelineAndOffsets(t *testing.T) {
	spec := acceptanceSpec(t)
	if len(spec.Representations) != 3 {
		t.Fatalf("representations=%d", len(spec.Representations))
	}
	for _, rep := range spec.Representations {
		if len(rep.Segments) != 120 || rep.AudioOffsetUS != -37_000 {
			t.Fatalf("rep=%s segments=%d offset=%d", rep.ID, len(rep.Segments), rep.AudioOffsetUS)
		}
		last := rep.Segments[len(rep.Segments)-1]
		if end := last.StartUS + last.DurationUS; end != 480_000_000 {
			t.Fatalf("rep=%s timeline end=%d", rep.ID, end)
		}
	}
}

func TestHoleFixtureKeepsSequentialGaps(t *testing.T) {
	spec, err := Generate(Options{Seed: 7, Representations: 1, Segments: 10, TargetUS: 4_000_000, JitterUS: 0, Holes: 2})
	if err != nil {
		t.Fatal(err)
	}
	rep := spec.Representations[0]
	if len(rep.Segments) != 8 {
		t.Fatalf("segments=%d", len(rep.Segments))
	}
	for i := 1; i < len(rep.Segments); i++ {
		if rep.Segments[i].Number <= rep.Segments[i-1].Number {
			t.Fatal("segment numbers are not increasing")
		}
	}
}

func TestMergeNewerWinsAndSameVersionConflicts(t *testing.T) {
	base, err := Generate(Options{Seed: 1, Representations: 1, Segments: 6, TargetUS: 4_000_000, JitterUS: 1_600, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := Generate(Options{Seed: 2, Representations: 1, Segments: 6, TargetUS: 4_000_000, JitterUS: 1_600, Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := Merge(base, newer)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Version != 2 || merged.Representations[0].Segments[2].StartUS != newer.Representations[0].Segments[2].StartUS {
		t.Fatal("newer period did not win overlap")
	}
	conflict := *newer
	conflict.Version = 1
	conflict.Representations = append([]Representation(nil), newer.Representations...)
	conflict.Representations[0].Segments = append([]Segment(nil), newer.Representations[0].Segments...)
	conflict.Representations[0].Segments[2].StartUS += 1
	if _, err := Merge(base, &conflict); err == nil {
		t.Fatal("expected same-version content conflict")
	}
}
