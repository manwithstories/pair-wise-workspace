package engine_test

import (
	"testing"

	"media-pipeline/internal/demo"
	"media-pipeline/internal/engine"
)

func TestRunDeterministicAndHealthy(t *testing.T) {
	dir := t.TempDir()
	cfg := engine.DefaultConfig()
	res1, err := engine.Run(42, dir, cfg, true)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res2, err := engine.Run(42, t.TempDir(), cfg, true)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if res1.Manifest.ContentSha != res2.Manifest.ContentSha {
		t.Fatalf("content sha differs across runs: %s vs %s",
			res1.Manifest.ContentSha, res2.Manifest.ContentSha)
	}
	if len(res1.Manifest.ContentSha) != 64 {
		t.Fatalf("sha256 length = %d, want 64 hex chars", len(res1.Manifest.ContentSha))
	}

	byName := map[string]engine.ScenarioManifest{}
	for _, r := range res1.Manifest.Scenarios {
		byName[r.Scenario] = r
	}
	normal := byName[demo.ScenNormal]
	if normal.Gaps != 0 {
		t.Fatalf("normal scenario gaps = %d, want 0", normal.Gaps)
	}
	if normal.Segments == 0 {
		t.Fatal("normal scenario produced no segments")
	}
	burst := byName[demo.ScenBurstLoss]
	if burst.Gaps == 0 {
		t.Fatal("burst-loss scenario should record gaps")
	}
	if len(burst.GapRanges) == 0 || burst.GapRanges[0].Frames == 0 {
		t.Fatal("burst-loss gap range not recorded")
	}
}

func TestCorruptionLocatedWithinOneByte(t *testing.T) {
	dir := t.TempDir()
	res, err := engine.Run(42, dir, engine.DefaultConfig(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Manifest.Scenarios {
		if r.Scenario != demo.ScenBadHeader {
			continue
		}
		if r.BadByte < 0 {
			t.Fatal("bad-header scenario did not report a byte offset")
		}
		// 注入点为 fmt 块 blockAlign 字段（偏移 32），验收要求相差不超过 1 字节。
		if d := r.BadByte - 32; d < -1 || d > 1 {
			t.Fatalf("bad byte %d not within 1 of injected 32", r.BadByte)
		}
	}
}

func TestUnalignedTailReported(t *testing.T) {
	res, err := engine.Run(7, t.TempDir(), engine.DefaultConfig(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Manifest.Scenarios {
		if r.Scenario == demo.ScenUnaligned && r.BadByte < 0 {
			t.Fatal("unaligned-tail scenario should report a fault offset")
		}
	}
}

func TestResumeProducesCheckpointAndTiming(t *testing.T) {
	dir := t.TempDir()
	res, err := engine.Run(42, dir, engine.DefaultConfig(), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResumeMS < 0 {
		t.Fatalf("resume ms = %d, must be non-negative", res.ResumeMS)
	}
	if res.Checkpt == nil || len(res.Checkpt.Sources) == 0 {
		t.Fatal("final checkpoint missing source identity")
	}
}

func TestReorderScenarioDedupes(t *testing.T) {
	res, err := engine.Run(42, t.TempDir(), engine.DefaultConfig(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Manifest.Scenarios {
		if r.Scenario == demo.ScenReorder && r.Duplicates == 0 {
			t.Fatal("reorder scenario should count at least one duplicate packet")
		}
	}
}
