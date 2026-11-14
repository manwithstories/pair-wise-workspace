package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"segmanifest/internal/align"
	"segmanifest/internal/fixture"
	"segmanifest/internal/ladder"
	"segmanifest/internal/manifest"
	"segmanifest/internal/verify"
)

func acceptanceManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	spec, err := fixture.Generate(fixture.Options{Seed: 7, Representations: 3, Segments: 120, TargetUS: 4_000_000, JitterUS: 1_600, AudioOffsetUS: -37_000})
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

func TestPrintPlanAcceptanceLine(t *testing.T) {
	m := acceptanceManifest(t)
	var output bytes.Buffer
	PrintPlan(&output, m, 121)
	want := "representations=3 segments=360 grid_points=121 max_drift_ms=1.43 p95_drift_ms=0.92 resync_points=0 discontinuities=0\n"
	if output.String() != want {
		t.Fatalf("got %q want %q", output.String(), want)
	}
}

func TestPrintVerifyText(t *testing.T) {
	result := verify.Verify(acceptanceManifest(t))
	var output bytes.Buffer
	if err := PrintVerify(&output, result, false); err != nil {
		t.Fatal(err)
	}
	want := "expanded_paths=360 duplicate_paths=0 holes=0 overlap_ms=0 roundtrip_ok=true\n"
	if output.String() != want {
		t.Fatalf("got %q want %q", output.String(), want)
	}
}

func TestStatsJSONIsStable(t *testing.T) {
	m := acceptanceManifest(t)
	var first, second bytes.Buffer
	if err := PrintStats(&first, m, true); err != nil {
		t.Fatal(err)
	}
	if err := PrintStats(&second, m, true); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() || !strings.HasPrefix(first.String(), `{"representations":[`) {
		t.Fatalf("unstable or unexpected JSON: %s", first.String())
	}
	var payload StatsJSON
	if err := json.Unmarshal(first.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Overall.MaxDriftMS != 1.43 || payload.Overall.P95DriftMS != 0.92 {
		t.Fatalf("overall=%+v", payload.Overall)
	}
}
