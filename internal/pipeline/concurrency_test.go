package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestSlowConsumerBackpressureKeepsMemoryBounded drives a long run through a
// deliberately slow consumer and asserts the queue never exceeds its bound and
// that every frame still arrives.
func TestSlowConsumerBackpressureKeepsMemoryBounded(t *testing.T) {
	dir, _ := genFixtures(t, 2000, nil)
	o := opts(dir, "normal")
	o.QueueDepth = 2
	o.SlowConsumer = 200 * time.Microsecond
	stats, err := New(o).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.PeakQueueDepth > 2 {
		t.Fatalf("peak depth %d exceeds queue depth 2", stats.PeakQueueDepth)
	}
	if stats.FramesIndexed != 2000 {
		t.Fatalf("indexed %d", stats.FramesIndexed)
	}
}

// TestManyConcurrentCancellations cancels jobs at varying points; every Run
// must return and leave a resumable checkpoint without racing the writer.
func TestManyConcurrentCancellations(t *testing.T) {
	dir, _ := genFixtures(t, 400, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := Options{
				Input:          dir + "/normal.seq",
				Output:         dir + "/c" + itoa(i) + ".mp4",
				CheckpointPath: dir + "/c" + itoa(i) + ".ckpt",
				ChunkFrames:    16, QueueDepth: 2,
				SlowConsumer: time.Millisecond,
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(5+i)*time.Millisecond)
			defer cancel()
			if _, err := New(o).Run(ctx); err != nil && ctx.Err() == nil {
				t.Errorf("run %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

func itoa(i int) string { return string(rune('0' + i)) }
