package pipeline_test

import (
	"context"
	"testing"
	"time"

	"mediapack/internal/codec"
	"mediapack/internal/fault"
	"mediapack/internal/fixture"
	"mediapack/internal/pipeline"
)

func newInput(t *testing.T, frames int) []byte {
	t.Helper()
	data, _, _, err := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H264, Seed: 17, Frames: frames, KeyEvery: 8, ExtraSize: 128,
	}, fixture.Fault{})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHappyPathMetrics(t *testing.T) {
	input := newInput(t, 60)
	p, err := pipeline.New(pipeline.Config{
		SourceID: "seed-17", ChunkBytes: 4096, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Metrics
	if m.Packets != 60 {
		t.Fatalf("packets = %d want 60", m.Packets)
	}
	if m.Fragments < 2 {
		t.Fatalf("expected multiple fragments, got %d", m.Fragments)
	}
	if m.OutputSHA256 == "" || m.SourceSHA256 == "" {
		t.Fatal("digests must be populated")
	}
	if m.PeakBufferBytes <= 0 {
		t.Fatalf("peak buffer not measured: %d", m.PeakBufferBytes)
	}
	// In-flight AU bytes are bounded by the chunk semaphore plus at most one
	// item already handed to the consumer.
	if m.PeakBufferBytes > int64(4096)+int64(8<<20) {
		t.Fatalf("peak buffer %d exceeds bounded expectation", m.PeakBufferBytes)
	}
	if m.Keyframes == 0 {
		t.Fatal("keyframes not counted")
	}
}

func TestCancellationUnblocksGoroutines(t *testing.T) {
	input := newInput(t, 40)
	p, _ := pipeline.New(pipeline.Config{
		SourceID: "s", ChunkBytes: 256,
		SlowConsumer: 50 * time.Millisecond, WorkDir: t.TempDir(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := p.Run(ctx, input); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		fe, ok := fault.As(err)
		if !ok || fe.Kind != fault.KindCanceled {
			t.Fatalf("expected cancellation fault, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("goroutines did not exit after cancellation")
	}
}

func TestInterruptAndResumeDeterministic(t *testing.T) {
	input := newInput(t, 60)
	dir := t.TempDir()
	cfg := pipeline.Config{
		SourceID: "seed-17", ChunkBytes: 1024,
		SlowConsumer: 5 * time.Millisecond, InterruptAt: 8, WorkDir: dir,
	}
	p1, _ := pipeline.New(cfg)
	_, err := p1.Run(context.Background(), input)
	if err == nil {
		t.Fatal("expected simulated interruption")
	}
	if _, ok := err.(*pipeline.ErrInterrupted); !ok {
		t.Fatalf("want ErrInterrupted, got %T %v", err, err)
	}

	// Resume with a fresh pipeline instance, no slow delay.
	cfg.SlowConsumer = 0
	cfg.InterruptAt = -1
	p2, _ := pipeline.New(cfg)
	start := time.Now()
	res, err := p2.Resume(context.Background(), input)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Metrics.Resumed {
		t.Fatal("resume flag not set")
	}
	if !res.Metrics.CacheRevalidated {
		t.Fatal("resume must revalidate at least one content-addressed cache entry")
	}
	if elapsed > 1000*time.Millisecond || res.Metrics.ResumeMs > 1000 {
		t.Fatalf("resume too slow: elapsed=%v metric=%dms", elapsed, res.Metrics.ResumeMs)
	}

	// A fresh full run over the same input, with identical byte budget, must
	// yield the same output digest.
	p3, _ := pipeline.New(pipeline.Config{
		SourceID: "seed-17", ChunkBytes: 1024, WorkDir: t.TempDir(),
	})
	full, err := p3.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if full.Metrics.OutputSHA256 != res.Metrics.OutputSHA256 {
		t.Fatalf("digest drift: resume=%s full=%s",
			res.Metrics.OutputSHA256, full.Metrics.OutputSHA256)
	}
}

func TestParseErrorOffsetPropagates(t *testing.T) {
	data, _, keyOffs, _ := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H264, Seed: 17, Frames: 10, KeyEvery: 5,
	}, fixture.Fault{})
	bad := data[:keyOffs[1]+10]
	p, _ := pipeline.New(pipeline.Config{
		SourceID: "s", ChunkBytes: 4096, WorkDir: t.TempDir(),
	})
	_, err := p.Run(context.Background(), bad)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCorruptInput || fe.Offset != int64(len(bad)) {
		t.Fatalf("expected corrupt offset %d, got %v", len(bad), err)
	}
}

func TestResumeRejectsDifferentInput(t *testing.T) {
	input := newInput(t, 20)
	dir := t.TempDir()
	p1, _ := pipeline.New(pipeline.Config{
		SourceID: "s", ChunkBytes: 1024, InterruptAt: 2, WorkDir: dir,
	})
	if _, err := p1.Run(context.Background(), input); err == nil {
		t.Fatal("expected interruption")
	}
	p2, _ := pipeline.New(pipeline.Config{
		SourceID: "s", ChunkBytes: 1024, WorkDir: dir,
	})
	other := append([]byte(nil), input...)
	other[len(other)-1] ^= 0xFF
	_, err := p2.Resume(context.Background(), other)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindChecksum {
		t.Fatalf("expected checksum fault on identity mismatch, got %v", err)
	}
}
