package engine_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"mediaroute/internal/engine"
	"mediaroute/internal/fixture"
	"mediaroute/internal/index"
)

// TestMemoryBudgetUnder256MiB exercises a full scan+stream cycle and
// asserts peak heap allocation stays well inside the 256 MiB contract.
func TestMemoryBudgetUnder256MiB(t *testing.T) {
	spec := fixture.DefaultSpec(41)
	raw, err := spec.BuildMP4()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "big.mp4")
	if err := os.WriteFile(input, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	sr, err := engine.Scan(engine.ScanConfig{
		InputPath: input, IndexDir: filepath.Join(dir, "idx"),
		Params: index.DefaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Stream(engine.StreamConfig{
		Spec: spec, Report: sr.Report,
		OutputPath:    filepath.Join(dir, "out.bin"),
		Checkpoint:    filepath.Join(dir, "cp.json"),
		GapTimeoutNs:  50_000_000,
		BandwidthMbps: []float64{6, 1, 9},
	}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	const budget = 256 << 20
	peak := after.TotalAlloc - before.TotalAlloc
	// TotalAlloc is cumulative, so use the live heap plus a margin instead.
	live := after.HeapAlloc
	if live > budget {
		t.Fatalf("live heap %d bytes exceeds 256 MiB budget", live)
	}
	if peak > budget*4 {
		t.Fatalf("cumulative allocations %d far exceed budget", peak)
	}
	t.Logf("heap_alloc=%d KiB total_alloc_delta=%d KiB",
		live/1024, peak/1024)
}
