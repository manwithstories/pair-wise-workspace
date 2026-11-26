package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pairwise/relay/internal/fixture"
	"github.com/pairwise/relay/internal/media"
	"github.com/pairwise/relay/internal/pipeline"
)

func TestInterruptResumeAndFullRunsProduceSameArtifactHash(t *testing.T) {
	ctx := context.Background()
	in := fixture.Generate(fixture.Config{Seed: 17, Rungs: 3, Segments: 24, FaultSuite: true})
	dir := t.TempDir()

	fresh := in
	fresh.InterruptAfter = 9
	stopped, err := pipeline.Run(ctx, fresh, dir)
	if err != nil {
		t.Fatalf("fresh interrupted run: %v", err)
	}
	if !stopped.Interrupted || stopped.Checkpoint.ProcessedSegments != 9 {
		t.Fatalf("stopped=%+v cp=%+v", stopped.Interrupted, stopped.Checkpoint)
	}
	if stopped.CacheStats.Quarantined == 0 {
		t.Fatal("forged cache origin was not quarantined in first run")
	}

	resumeIn := in
	resumeIn.Resume = true
	resumed, err := pipeline.Run(ctx, resumeIn, dir)
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if resumed.Aligned != 24 || resumed.Segments != 24 || resumed.Renditions != 3 {
		t.Fatalf("counts aligned=%d segments=%d rungs=%d", resumed.Aligned, resumed.Segments, resumed.Renditions)
	}
	if resumed.DamageOffsetDelta != 0 || resumed.DamageChecked == 0 {
		t.Fatalf("damage checked=%d delta=%d", resumed.DamageChecked, resumed.DamageOffsetDelta)
	}
	if len(resumed.ArtifactSHA256) != 64 {
		t.Fatalf("artifact hash %q", resumed.ArtifactSHA256)
	}
	if resumed.TransportStats.Duplicates == 0 || resumed.TransportStats.OutOfOrder == 0 {
		t.Fatalf("transport stats=%+v", resumed.TransportStats)
	}

	verified, err := pipeline.Verify(ctx, in, dir)
	if err != nil {
		t.Fatalf("verify after resume: %v", err)
	}
	if verified.ArtifactSHA256 != resumed.ArtifactSHA256 {
		t.Fatal("verify hash differs from resumed run")
	}
	if verified.CacheStats.Rehashes == 0 || verified.CacheStats.Hits == 0 {
		t.Fatalf("cache verification stats=%+v", verified.CacheStats)
	}

	full, err := pipeline.Run(ctx, in, filepath.Join(dir, "full-reference"))
	if err != nil {
		t.Fatalf("full run: %v", err)
	}
	if full.ArtifactSHA256 != resumed.ArtifactSHA256 {
		t.Fatalf("full hash %s != resumed %s", full.ArtifactSHA256, resumed.ArtifactSHA256)
	}
}

func TestVerifyReturnsExitCode4AfterArtifactTampering(t *testing.T) {
	ctx := context.Background()
	in := fixture.Generate(fixture.Config{Seed: 17, Rungs: 3, Segments: 24, FaultSuite: false})
	dir := t.TempDir()
	if _, err := pipeline.Run(ctx, in, dir); err != nil {
		t.Fatal(err)
	}
	master := filepath.Join(dir, "master.m3u8")
	if err := os.WriteFile(master, []byte("# tampered\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := pipeline.Verify(ctx, in, dir)
	if code := media.ExitCodeOf(err); code != 4 {
		t.Fatalf("verify tamper exit=%d err=%v", code, err)
	}
}

func TestRejectOversizedSegmentCount(t *testing.T) {
	in := fixture.Generate(fixture.Config{Seed: 1, Rungs: 3, Segments: 24})
	in.Segments = 10001
	_, err := pipeline.Run(context.Background(), in, t.TempDir())
	if code := media.ExitCodeOf(err); code != 2 {
		t.Fatalf("exit=%d err=%v want 2", code, err)
	}
}
