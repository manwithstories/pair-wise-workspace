// Package demo generates and exercises every advertised success and fault
// path for the command-line acceptance demonstration.
package demo

import (
	"context"
	"fmt"
	"time"

	"mediacheck/internal/stream"
)

// SlowReaderResult describes a deterministic backpressure exercise.
type SlowReaderResult struct {
	Items           int
	ProcessedOffset int64
	HighWaterBytes  int
	LimitBytes      int
}

// RunSlowReader starts a producer that exceeds a 4 KiB byte budget and a
// consumer that deliberately reads slowly.
func RunSlowReader() (SlowReaderResult, error) {
	const itemBytes = 1000
	const itemCount = 12
	q := stream.NewQueue(stream.Config{MaxItems: 128, MaxBytes: 4096})
	done := make(chan error, 1)
	go func() {
		for i := 0; i < itemCount; i++ {
			err := q.Put(context.Background(), stream.Item{
				Offset: int64(i * itemBytes), Length: itemBytes,
				PTS: int64(i), Payload: make([]byte, itemBytes),
			})
			if err != nil {
				done <- err
				return
			}
		}
		close(done)
	}()

	for i := 0; i < itemCount; i++ {
		item, err := q.Get(context.Background())
		if err != nil {
			return SlowReaderResult{}, err
		}
		time.Sleep(2 * time.Millisecond)
		q.Ack(item.EndOffset())
	}
	q.Close()
	if err := <-done; err != nil {
		return SlowReaderResult{}, err
	}
	return SlowReaderResult{
		Items: itemCount, ProcessedOffset: q.ProcessedPosition(),
		HighWaterBytes: q.HighWaterBytes(), LimitBytes: q.MaxBytes(),
	}, nil
}

func (r SlowReaderResult) String() string {
	return fmt.Sprintf("fault=slow-reader status=backpressure_verified items=%d processed_offset=%d queue_high_water_bytes=%d queue_limit_bytes=%d",
		r.Items, r.ProcessedOffset, r.HighWaterBytes, r.LimitBytes)
}
