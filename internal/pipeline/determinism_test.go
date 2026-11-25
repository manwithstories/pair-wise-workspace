package pipeline_test

import (
	"context"
	"testing"

	"avcheck/internal/pipeline"
	"avcheck/internal/report"
)

// TestSameSeedIsReproducible requires identical metrics and digests for the
// same seed, the acceptance rule for scripted demos.
func TestSameSeedIsReproducible(t *testing.T) {
	data, sc := buildCase(t, pipeline.CaseBurstReorderSlow)
	build := func() *report.Manifest {
		cfg := testConfig(t)
		cfg.Link = sc.Link
		runner, err := pipeline.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		res, err := runner.RunFromBytes(context.Background(), data, t.TempDir())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if !res.Committed {
			t.Fatalf("run not committed: %v", res.Faults)
		}
		return res.Manifest
	}
	a, b := build(), build()
	if a.SHA256 != b.SHA256 {
		t.Fatalf("digests differ: %s vs %s", a.SHA256, b.SHA256)
	}
	if a.ActualBps != b.ActualBps || a.RecoveredPackets != b.RecoveredPackets {
		t.Fatal("metrics differ across identical seeds")
	}
}

// TestDifferentSeedChangesPattern proves the seed actually drives the link
// model rather than being cosmetic.
func TestDifferentSeedChangesPattern(t *testing.T) {
	data, sc := buildCase(t, pipeline.CaseBurstReorderSlow)
	run := func(seed int64) *report.Manifest {
		cfg := testConfig(t)
		cfg.Seed = seed
		cfg.Link = sc.Link
		runner, err := pipeline.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		res, err := runner.RunFromBytes(context.Background(), data, t.TempDir())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return res.Manifest
	}
	a, b := run(731), run(732)
	if a.SHA256 == b.SHA256 {
		t.Fatal("different seeds produced identical manifests")
	}
}
