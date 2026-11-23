package demo_test

import (
	"os"
	"testing"

	"mediaroute/internal/demo"
)

func TestDemoFullScenario(t *testing.T) {
	opt := demo.Options{
		Seed: 41, DropBurst: 8, DropBurstAt: 30, Duplicate: 3,
		ReorderWindow: 12, BandwidthMbps: []float64{6, 1, 9},
		InterruptAfter: 120, Resume: true,
	}
	res, err := demo.Run(opt)
	if err != nil {
		t.Fatalf("demo run: %v", err)
	}
	defer os.RemoveAll(res.WorkDir)
	for _, d := range res.ExtraDirs {
		defer os.RemoveAll(d)
	}

	// Healthy phase: ~1e3 samples, ~1e3 ms keyframe p95, zero damage.
	if res.Healthy.Scan.Samples != 1000 {
		t.Fatalf("samples=%d want 1000", res.Healthy.Scan.Samples)
	}
	if res.Healthy.Keyframe.IntervalP95MS < 900 ||
		res.Healthy.Keyframe.IntervalP95MS > 1100 {
		t.Fatalf("p95=%d not near 1000ms", res.Healthy.Keyframe.IntervalP95MS)
	}
	if res.Healthy.Damage.OffsetErrorSamples != 0 {
		t.Fatal("healthy run must have zero offset error")
	}
	if res.Healthy.Scan.IndexRejected {
		t.Fatal("first scan with no index must not be called a rejection")
	}

	// Damage phase: packet layer propagated to samples, error <= 1.
	if res.Damaged.Damage.Gaps == 0 {
		t.Fatal("damage phase detected no gaps")
	}
	if res.Damaged.Damage.SampleCount == 0 {
		t.Fatal("packet gaps did not propagate to sample damage")
	}
	if res.Damaged.Damage.OffsetErrorSamples > 1 {
		t.Fatalf("offset error %d exceeds one sample",
			res.Damaged.Damage.OffsetErrorSamples)
	}

	// Resume phase: fast, bounded replay, hash equal to independent full run.
	if res.Resume == nil || res.Full == nil {
		t.Fatal("resume/full phases missing")
	}
	if res.Resume.Resume.ElapsedMS >= 5000 {
		t.Fatalf("resume elapsed %dms >= 5000", res.Resume.Resume.ElapsedMS)
	}
	if res.Resume.Resume.ReplayedSamples > res.ExpectedUncommitted {
		t.Fatalf("replayed %d > uncommitted %d",
			res.Resume.Resume.ReplayedSamples, res.ExpectedUncommitted)
	}
	if res.Resume.Output.SHA256 != res.Full.Output.SHA256 {
		t.Fatal("resumed and full-run output hashes differ")
	}
	if len(res.Full.Output.SHA256) != 64 {
		t.Fatal("output hash is not 64 hex chars")
	}
}

func TestDemoWithoutResume(t *testing.T) {
	opt := demo.Options{
		Seed: 7, DropBurst: 4, DropBurstAt: 10,
		BandwidthMbps: []float64{6, 1, 9}, InterruptAfter: 50, Resume: false,
	}
	res, err := demo.Run(opt)
	if err != nil {
		t.Fatalf("demo run: %v", err)
	}
	defer os.RemoveAll(res.WorkDir)
	if res.Resume != nil {
		t.Fatal("resume phase should be absent when --resume is off")
	}
	if res.Damaged.Damage.Gaps == 0 {
		t.Fatal("damage phase expected gaps")
	}
}
