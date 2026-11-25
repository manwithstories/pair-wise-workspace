package verify

import (
	"context"
	"runtime"
	"time"
)

// heapSampler records the maximum live Go heap allocation.
type heapSampler struct {
	peak uint64
}

// runHeapSampler runs until the returned cancel is called and done closes.
func runHeapSampler(parent context.Context) (*heapSampler, context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	s := &heapSampler{}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s.peak = ms.HeapAlloc

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > s.peak {
					s.peak = ms.HeapAlloc
				}
				return
			case <-ticker.C:
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > s.peak {
					s.peak = ms.HeapAlloc
				}
			}
		}
	}()
	return s, cancel, done
}

// MiB converts bytes to binary megabytes.
func MiB(b uint64) float64 { return float64(b) / (1024.0 * 1024.0) }
