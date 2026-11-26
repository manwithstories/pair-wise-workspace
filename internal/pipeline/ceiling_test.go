package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMemoryCeilingUnderContention hammers the semaphore from many goroutines
// with a deliberately small capacity and asserts the in-use byte count never
// exceeds the ceiling while every grant is eventually served under backpressure.
func TestMemoryCeilingUnderContention(t *testing.T) {
	const cap = 4096
	sem := newMemSemaphore(cap)
	ctx := context.Background()

	var wg sync.WaitGroup
	var violations atomic.Int32
	var maxObserved atomic.Int64
	workers, grantsPer := 24, 50
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for g := 0; g < grantsPer; g++ {
				n := 1 + (g*37)%512 // varied sizes, some large relative to cap
				if err := sem.Acquire(ctx, n); err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				used := sem.InUse()
				if used > cap {
					violations.Add(1)
				}
				for {
					cur := maxObserved.Load()
					if used <= cur || maxObserved.CompareAndSwap(cur, used) {
						break
					}
				}
				// Hold briefly to create real contention, then release.
				time.Sleep(time.Microsecond)
				sem.Release(n)
			}
		}()
	}
	wg.Wait()
	if violations.Load() != 0 {
		t.Fatalf("in-use exceeded capacity %d times", violations.Load())
	}
	if maxObserved.Load() > cap {
		t.Fatalf("max observed %d > cap %d", maxObserved.Load(), cap)
	}
	if sem.InUse() != 0 {
		t.Fatalf("all grants released but in-use = %d", sem.InUse())
	}
}

// TestDefaultCapIsSixteenMiB pins the documented archive ceiling.
func TestDefaultCapIsSixteenMiB(t *testing.T) {
	if MaxInFlightBytes != 16*1024*1024 {
		t.Fatalf("MaxInFlightBytes = %d, want 16777216", MaxInFlightBytes)
	}
}
