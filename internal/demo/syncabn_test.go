package demo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediaroute/internal/fixtures"
	"mediaroute/internal/manifest"
	"mediaroute/internal/scan"
)

func TestSyncAbnormalDetection(t *testing.T) {
	dir := t.TempDir()
	a, err := fixtures.Build(dir, fixtures.DefaultSpec(42), fixtures.VariantNormal)
	if err != nil {
		t.Fatal(err)
	}
	// Shift the audio origin by 35 ms; the deviation must be flagged while the
	// recorded times remain the source truth.
	raw, err := os.ReadFile(a.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(dir, "shifted-manifest.json")
	if err := os.WriteFile(edited, []byte(mustRaw(t, string(raw), `"normal-audio.mp4",
      "required": true,
      "origin_ms": 1234`, `"normal-audio.mp4",
      "required": true,
      "origin_ms": 1269`)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := scan.Run(context.Background(), mustManifest(t, edited), scan.Options{BaseDir: dir, VerifyBytes: true})
	if err != nil {
		t.Fatal(err)
	}
	take, err := process(context.Background(), dir, res, Options{ReorderWindow: 64})
	if err != nil {
		t.Fatal(err)
	}
	if take.Report.SyncAbnormalCount == 0 {
		t.Fatal("35 ms shift must produce abnormal sync samples")
	}
	var sawAbnormal bool
	for _, s := range take.Report.SyncSamples {
		if s.Abnormal && s.Deviation >= 20 {
			sawAbnormal = true
		}
	}
	if !sawAbnormal {
		t.Fatal("per-sample audit did not retain an abnormal deviation")
	}
}

func mustRaw(t *testing.T, raw, old, new string) string {
	t.Helper()
	if old != "" {
		if !strings.Contains(raw, old) {
			t.Fatalf("manifest template not found")
		}
		return strings.Replace(raw, old, new, 1)
	}
	return raw
}

func mustManifest(t *testing.T, path string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
