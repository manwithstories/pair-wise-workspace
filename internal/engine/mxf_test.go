package engine_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/engine"
	"mediaroute/internal/fixture"
	"mediaroute/internal/index"
)

// TestMXFEndToEnd drives an MXF asset through scan, index and stream,
// proving the MXF-flavoured KLV container is a first-class input rather
// than only being readable at the probe layer.
func TestMXFEndToEnd(t *testing.T) {
	spec := fixture.DefaultSpec(99)
	raw, err := spec.BuildMXF()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "asset.mxf")
	if err := os.WriteFile(input, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	idxDir := filepath.Join(dir, "idx")
	sr, err := engine.Scan(engine.ScanConfig{
		InputPath: input, IndexDir: idxDir, Params: index.DefaultParams(),
	})
	if err != nil {
		t.Fatalf("MXF scan: %v", err)
	}
	if sr.Report.Format != "mxf" {
		t.Fatalf("format=%s want mxf", sr.Report.Format)
	}
	if sr.Report.TotalSamples() != spec.SampleCount {
		t.Fatalf("MXF samples=%d want %d",
			sr.Report.TotalSamples(), spec.SampleCount)
	}
	st, err := engine.Stream(engine.StreamConfig{
		Spec: spec, Report: sr.Report,
		OutputPath:   filepath.Join(dir, "mxf.out"),
		Checkpoint:   filepath.Join(dir, "mxf.cp"),
		GapTimeoutNs: 50_000_000,
	})
	if err != nil {
		t.Fatalf("MXF stream: %v", err)
	}
	if st.Committed != spec.SampleCount {
		t.Fatalf("MXF committed=%d want %d", st.Committed, spec.SampleCount)
	}
	if len(st.OutputSHA256) != 64 {
		t.Fatal("MXF output missing 64-char hash")
	}

	// A second scan must reuse the persisted MXF index.
	sr2, err := engine.Scan(engine.ScanConfig{
		InputPath: input, IndexDir: idxDir, Params: index.DefaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sr2.Reused {
		t.Fatal("MXF index was not reused on second scan")
	}
}
