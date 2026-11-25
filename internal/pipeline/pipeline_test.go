package pipeline_test

import (
	"path/filepath"
	"testing"

	"mediqc/internal/audit"
	"mediqc/internal/media"
	"mediqc/internal/pipeline"
)

func TestFullSuiteVerifiesAndRepeatsDeterministically(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	opts := pipeline.Options{Seed: 412, Suite: "all", Out: dir}
	m, err := pipeline.Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if m.Cases != 6 || m.Clean != 1 || m.Repaired != 4 || m.Rejected != 1 {
		t.Fatalf("counts cases=%d clean=%d repaired=%d rejected=%d", m.Cases, m.Clean, m.Repaired, m.Rejected)
	}
	if m.CCGaps != 1 || m.PSISwitches != 1 || m.PCRWraps != 1 || m.LateReplays != 1 {
		t.Fatalf("damage metrics=%+v", m)
	}
	if abs64(m.BitrateDeltaPPM) > 5000 {
		t.Fatalf("bitrate ppm=%d", m.BitrateDeltaPPM)
	}
	if m.SampleHashMatch != "100%" || m.ManifestSkewMS > 1 || !m.ReplayHashEqual {
		t.Fatalf("audit quality fields: hash=%s skew=%d replay=%v", m.SampleHashMatch, m.ManifestSkewMS, m.ReplayHashEqual)
	}

	v1, verr := audit.VerifyDirectory(dir)
	if verr != nil {
		t.Fatal(verr)
	}
	if _, err := pipeline.Run(opts); err != nil {
		t.Fatal(err)
	}
	v2, verr := audit.VerifyDirectory(dir)
	if verr != nil {
		t.Fatal(verr)
	}
	if v1.ManifestHash != v2.ManifestHash {
		t.Fatalf("manifest hashes differ: %s != %s", v1.ManifestHash, v2.ManifestHash)
	}
}

func TestUnsupportedSuiteClassified(t *testing.T) {
	_, err := pipeline.Run(pipeline.Options{Seed: 1, Suite: "4k-hdr", Out: t.TempDir()})
	if err == nil || err.Code != media.CodeUnsupported {
		t.Fatalf("expected unsupported error, got %v", err)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
