package demo

import (
	"context"
	"runtime"
	"testing"
)

func heapAfterRun(t *testing.T, scale int) uint64 {
	t.Helper()
	_, err := Run(context.Background(), Options{Seed: 42, ReorderWindow: 64, Quiet: true, DurationScale: scale})
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func TestLiveHeapBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("long-running duration scaling check")
	}
	small := heapAfterRun(t, 2)
	large := heapAfterRun(t, 12)
	t.Logf("live heap small=%d large=%d", small, large)
	// Live heap after a run stays flat: streaming buffers and the collector are
	// bounded, and the fixture generator streams payloads to disk.
	if large > small+4<<20 {
		t.Fatalf("live heap grew with duration: small=%d large=%d", small, large)
	}
}
