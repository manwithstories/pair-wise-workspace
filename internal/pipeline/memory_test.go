package pipeline

import (
	"context"
	"runtime"
	"testing"
)

func TestMemoryUnderCeiling(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Frames = 600
	cfg.Seed = 7
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := NewEngine(cfg).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	peak := after.TotalAlloc - before.TotalAlloc
	const limit = 64 << 20
	t.Logf("600-frame total alloc = %.2f MiB (ceiling 64 MiB)", float64(peak)/(1<<20))
	if peak > limit {
		t.Fatalf("allocation %d exceeds 64 MiB ceiling", peak)
	}
}
