package main

import (
	"context"
	"regexp"
	"testing"

	"archive/internal/media"
)

func mustRun(t *testing.T, opts options) *runResult {
	t.Helper()
	res, err := runPipeline(context.Background(), opts)
	if err != nil {
		t.Fatalf("runPipeline(%+v): %v", opts, err)
	}
	return res
}

func TestPipelineDamaged(t *testing.T) {
	res := mustRun(t, options{kase: "damaged", seed: 7, workers: 4, runs: 1})
	m := res.metrics
	if m.Frames != 90 {
		t.Fatalf("frames=%d, want 90", m.Frames)
	}
	if m.LossRecovered != 18 || m.LossTotal != 20 {
		t.Fatalf("loss_recovery=%d/%d, want 18/20", m.LossRecovered, m.LossTotal)
	}
	if got := m.RecoveryRate(); got < 0.899 || got > 0.901 {
		t.Fatalf("recovery_rate=%.3f, want 0.900", got)
	}
	if m.AVSyncMaxUs > 20.9 {
		t.Fatalf("av_sync_max_us=%.3f exceeds one 48 kHz sample", m.AVSyncMaxUs)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(m.ManifestSHA) {
		t.Fatalf("manifest_sha256=%q not 64 hex chars", m.ManifestSHA)
	}
	if len(res.faults) == 0 {
		t.Fatal("damaged case must record the truncated-box fault")
	}
	if len(res.decisions) == 0 {
		t.Fatal("damaged case must record metadata resolution rules")
	}
}

func TestPipelineClean(t *testing.T) {
	res := mustRun(t, options{kase: "clean", seed: 1, workers: 2, runs: 1})
	m := res.metrics
	if m.Frames != 90 || m.LossTotal != 0 || m.RecoveryRate() != 1 {
		t.Fatalf("clean metrics = %+v", m)
	}
	if len(res.faults) != 0 {
		t.Fatalf("clean case faults = %v", res.faults)
	}
}

// The manifest digest must not depend on the worker count or on how
// many times the pipeline is re-run.
func TestPipelineDeterminism(t *testing.T) {
	var hashes []string
	for _, w := range []int{1, 2, 4, 8} {
		res := mustRun(t, options{kase: "damaged", seed: 7, workers: w, runs: 1})
		hashes = append(hashes, res.metrics.ManifestSHA)
	}
	for i := 0; i < 2; i++ { // reruns with the same settings
		res := mustRun(t, options{kase: "damaged", seed: 7, workers: 4, runs: 1})
		hashes = append(hashes, res.metrics.ManifestSHA)
	}
	for i, h := range hashes[1:] {
		if h != hashes[0] {
			t.Fatalf("hash mismatch: run 0 = %s, run %d = %s", hashes[0], i+1, h)
		}
	}
}

func TestPipelineInvalidMetadata(t *testing.T) {
	_, err := runPipeline(context.Background(), options{kase: "invalid", seed: 1, workers: 1})
	if err == nil {
		t.Fatal("expected invalid-metadata failure")
	}
	f, ok := media.AsFault(err)
	if !ok || f.Stage != media.StageNormalize {
		t.Fatalf("want normalize fault, got %v", err)
	}
}

func TestPipelineCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runPipeline(ctx, options{kase: "clean", seed: 1, workers: 2})
	if err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
