package run

import (
	"context"
	"runtime"
	"testing"
)

// TestRunLargeFixtureMemoryBounded runs the full acceptance-sized dataset and
// checks that the pipeline's in-flight bound holds and memory stays modest.
func TestRunLargeFixtureMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	in, fx := fixture(t, 1200, 137)
	out := t.TempDir()
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	rep, err := Run(context.Background(), Config{
		InputDir: in, OutDir: out, Ladder: testLadder(t), QueueCap: 8, RandomSeeks: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	if rep.PeakInflight > 24 {
		t.Fatalf("peak inflight=%d", rep.PeakInflight)
	}
	if rep.CorruptRecords != fx.TotalCorrupt() {
		t.Fatalf("corrupt=%d want %d", rep.CorruptRecords, fx.TotalCorrupt())
	}
	if rep.SegmentSHAChecked == 0 || rep.AlignmentError != 0 || rep.KeyframeHitRate != 1 {
		t.Fatalf("report: %+v", rep)
	}
	// The six-camera, 7200-frame delivery must not need anything close to
	// its on-disk payload size in heap. Allow generous headroom for the Go
	// runtime but catch an order-of-magnitude regression.
	const limit = 256 << 20
	if after.TotalAlloc-before.TotalAlloc > limit {
		t.Fatalf("total allocations=%d MiB exceeds %d MiB",
			(after.TotalAlloc-before.TotalAlloc)>>20, limit>>20)
	}
	t.Logf("frames=%d segs=%d heap_alloc=%.1fMiB total_alloc=%.1fMiB",
		rep.FramesParsed, rep.Segments,
		float64(after.HeapAlloc)/(1<<20), float64(after.TotalAlloc-before.TotalAlloc)/(1<<20))
}
