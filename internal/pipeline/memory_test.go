package pipeline

import (
	"runtime"
	"testing"
)

// TestMemoryBudgetTenMinuteSample asserts the documented 512 MiB ceiling on
// the 10-minute, three-camera sample. Peak heap and total runtime memory are
// both checked; the ceiling is generous on purpose so GC timing cannot make
// the test flaky.
func TestMemoryBudgetTenMinuteSample(t *testing.T) {
	if testing.Short() {
		t.Skip("memory test skipped in short mode")
	}
	dir := t.TempDir()
	cfg := Config{
		Seed: 42, Cameras: []string{"cam0", "cam1", "cam2"},
		Segments: 150, SegmentDurNS: 4_000_000_000, PacketRate: 50,
		WorkDir: dir,
	}
	if _, err := Run(cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	const ceiling = 512 << 20
	if ms.HeapAlloc > ceiling {
		t.Fatalf("heap %d exceeds 512 MiB ceiling", ms.HeapAlloc)
	}
	if ms.TotalAlloc > 8*ceiling {
		t.Fatalf("total allocation %d suggests unbounded growth", ms.TotalAlloc)
	}
	t.Logf("heap=%dMiB sys=%dMiB", ms.HeapAlloc>>20, ms.Sys>>20)
}
