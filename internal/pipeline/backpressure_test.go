package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/archive/tsrestore/internal/cache"
	"github.com/archive/tsrestore/internal/checkpoint"
	"github.com/archive/tsrestore/internal/sample"
)

func TestSemaphoreBlocksAtCapacityThenReleases(t *testing.T) {
	sem := newMemSemaphore(100)
	ctx := context.Background()
	if err := sem.Acquire(ctx, 60); err != nil {
		t.Fatal(err)
	}
	if err := sem.Acquire(ctx, 40); err != nil {
		t.Fatal(err)
	}
	if sem.InUse() != 100 {
		t.Fatalf("in-use = %d, want 100", sem.InUse())
	}

	// A further 10-byte grant must block (backpressure), then unblock on release.
	acquired := make(chan error, 1)
	go func() { acquired <- sem.Acquire(ctx, 10) }()
	select {
	case <-acquired:
		t.Fatal("Acquire must block while at capacity")
	case <-time.After(30 * time.Millisecond):
	}
	sem.Release(10)
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("blocked acquire should succeed after release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire never unblocked after release")
	}
	if sem.PeakBytes() != 100 {
		t.Fatalf("peak = %d, want 100", sem.PeakBytes())
	}
}

func TestSemaphoreCancellationUnblocks(t *testing.T) {
	sem := newMemSemaphore(8)
	ctx, cancel := context.WithCancel(context.Background())
	if err := sem.Acquire(context.Background(), 8); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- sem.Acquire(ctx, 4) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock Acquire")
	}
}

func TestOversizedReservationRejected(t *testing.T) {
	sem := newMemSemaphore(10)
	err := sem.Acquire(context.Background(), 11)
	if err == nil {
		t.Fatal("oversized reservation must be rejected, not deadlocked")
	}
}

// buildInputs synthesizes the two-rendition damaged fixture for a short program.
func buildInputs(t *testing.T, seed int64, secs int) []*RenditionInput {
	t.Helper()
	cfg := sample.DamageConfig{Loss: 0.02, ReorderDist: 9, Duplicates: 0.02}
	var ins []*RenditionInput
	for i, r := range sample.DefaultRenditions() {
		stream, _, err := sample.BuildRendition(r, sample.ProgramDuration(secs),
			seed+int64(i), cfg)
		if err != nil {
			t.Fatal(err)
		}
		ins = append(ins, &RenditionInput{
			Name: r.Name, SSRC: r.SSRC, Bandwidth: r.Bandwidth,
			Width: r.Width, Height: r.Height, Datagrams: stream,
		})
	}
	return ins
}

func TestRunWithSlowConsumerStaysUnderCapAndNoDrops(t *testing.T) {
	dir := t.TempDir()
	cas, err := cache.Open(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	ckpt, err := checkpoint.Open(filepath.Join(dir, "ckpt.json"), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	// Small artificial delay forces repeated backpressure while keeping the
	// real 16 MiB ceiling enforced by the pipeline semaphore.
	delay := func() { time.Sleep(time.Millisecond) }
	res, err := Run(context.Background(), buildInputs(t, 41, 24), ckpt, cas,
		Config{OutDir: dir, Seed: 41, ProgramSecs: 24, SlowConsumerDelay: delay})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.PeakBufferBytes > MaxInFlightBytes {
		t.Fatalf("peak %d exceeds cap %d", res.PeakBufferBytes, MaxInFlightBytes)
	}
	if len(res.Renditions) != 2 {
		t.Fatalf("renditions = %d, want 2", len(res.Renditions))
	}
	// Each 24 s program yields four 6 s segments. No data is dropped.
	for _, rr := range res.Renditions {
		if len(rr.Segments) != 4 {
			t.Fatalf("%s segments = %d, want 4", rr.Name, len(rr.Segments))
		}
	}
}

func TestRunCancellationStopsCleanly(t *testing.T) {
	dir := t.TempDir()
	cas, _ := cache.Open(filepath.Join(dir, "cas"))
	ckpt, _ := checkpoint.Open(filepath.Join(dir, "ckpt.json"), []byte("secret"))
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel almost immediately; the run must return rather than hang.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	_, err := Run(ctx, buildInputs(t, 41, 60), ckpt, cas,
		Config{OutDir: dir, Seed: 41, ProgramSecs: 60})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
