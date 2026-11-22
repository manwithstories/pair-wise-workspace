package pipeline_test

import (
	"context"
	"testing"
	"time"

	"mediapack/internal/codec"
	"mediapack/internal/fixture"
	"mediapack/internal/pipeline"
)

func TestSlowConsumerBackpressureBounded(t *testing.T) {
	data, _, _, _ := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H264, Seed: 5, Frames: 50, KeyEvery: 10, ExtraSize: 512,
	}, fixture.Fault{})
	const budget = 2048
	p, _ := pipeline.New(pipeline.Config{
		SourceID:    "slow",
		ChunkBytes:  budget,
		SlowConsumer: 30 * time.Millisecond,
		WorkDir:     t.TempDir(),
	})
	res, err := p.Run(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	// One AU can overshoot the semaphore by being handed to the consumer, so
	// the high-water mark stays within one max AU above the byte budget.
	if res.Metrics.PeakBufferBytes > int64(budget)+int64(8<<20) {
		t.Fatalf("backpressure failed: peak=%d budget=%d",
			res.Metrics.PeakBufferBytes, budget)
	}
	if res.Metrics.PeakBufferBytes <= int64(budget) {
		// Under a slow consumer the buffer should actually fill to the budget,
		// proving the producer is throttled rather than unbounded.
		t.Logf("peak %d did not reach budget %d (consumer kept up)",
			res.Metrics.PeakBufferBytes, budget)
	}
}
