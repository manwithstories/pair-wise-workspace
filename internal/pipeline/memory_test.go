package pipeline

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// fixedOverhead is the allowance the delivery contract grants on top of the
// configured payload budget.
const fixedOverhead = 8 << 20

// TestMemoryStaysWithinBudget measures live heap during a run and asserts it
// never exceeds the queue budget plus the fixed overhead. The measurement is
// conservative: it takes the peak of a sampling loop, not a post-hoc heap
// profile.
func TestMemoryStaysWithinBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("memory sampling is timing sensitive")
	}
	opts := testOptions(t)
	opts.ChunkBytes = 64 << 10
	opts.MemoryLimitBytes = 256 << 10
	opts.ConsumerDelayMs = 1

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	stop := make(chan struct{})
	peak := make(chan uint64, 1)
	go func() {
		var max uint64
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				peak <- max
				return
			default:
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > max {
				max = ms.HeapAlloc
			}
			// Sampling must not starve the pipeline under test.
			time.Sleep(5 * time.Millisecond)
		}
	}()

	if _, err := Run(context.Background(), opts); err != nil {
		close(stop)
		t.Fatal(err)
	}
	close(stop)
	high := <-peak
	budget := uint64(opts.MemoryLimitBytes) + fixedOverhead
	if high > budget {
		t.Fatalf("peak live heap %d exceeds budget %d", high, budget)
	}
	t.Logf("peak live heap %d bytes (budget %d)", high, budget)
}
