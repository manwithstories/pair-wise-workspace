package verify

import (
	"testing"

	"segmanifest/internal/align"
	"segmanifest/internal/fixture"
	"segmanifest/internal/ladder"
	"segmanifest/internal/manifest"
)

func verifiedManifest(t *testing.T, holes int) *manifest.Manifest {
	t.Helper()
	spec, err := fixture.Generate(fixture.Options{
		Seed: 7, Representations: 3, Segments: 120, TargetUS: 4_000_000,
		JitterUS: 1_600, AudioOffsetUS: -37_000, Holes: holes,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := spec.ToManifest(4_000_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	grid, err := ladder.BuildGrid(m.Representations, 4_000_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := align.Align(m, grid); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVerifyAcceptanceManifest(t *testing.T) {
	result := Verify(verifiedManifest(t, 0))
	if result.ExpandedPaths != 360 || result.DuplicatePaths != 0 || result.Holes != 0 || result.OverlapMS != 0 || !result.RoundtripOK || !result.OK {
		t.Fatalf("result=%+v", result)
	}
}

func TestVerifyDetectsHoles(t *testing.T) {
	result := Verify(verifiedManifest(t, 1))
	if result.Holes != 3 || result.OK {
		t.Fatalf("result=%+v", result)
	}
}

func TestVerifyDetectsDuplicateExpandedPaths(t *testing.T) {
	m := verifiedManifest(t, 0)
	m.Template = "seg/$Number%05d$.m4s"
	result := Verify(m)
	if result.DuplicatePaths != 240 || result.OK {
		t.Fatalf("result=%+v", result)
	}
}

func TestVerifyDetectsOverlapAndBadLastDuration(t *testing.T) {
	m := verifiedManifest(t, 0)
	rep := &m.Representations[0]
	rep.Segments[0].DurationUS += 1_000
	last := len(rep.Segments) - 1
	rep.Segments[last].DurationUS += 3_000
	result := Verify(m)
	if result.OverlapMS != 1 || result.OK {
		t.Fatalf("result=%+v", result)
	}
	if len(result.Problems) == 0 {
		t.Fatal("expected duration problem")
	}
}
