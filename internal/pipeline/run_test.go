package pipeline

import (
	"context"
	"testing"
	"time"

	"streamcut/internal/fixture"
	"streamcut/internal/media"
)

// buildPackets generates a clean deterministic stream for pipeline tests.
func buildPackets(t *testing.T) [][]byte {
	t.Helper()
	cfg := fixture.DefaultConfig(17, 4_000_000, fixture.CaseClean)
	res, err := fixture.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res.Packets
}

// TestRunProducesSegmentsAndStableHash exercises the full bounded graph and
// asserts two independent runs publish identical segment ids and hashes.
func TestRunProducesSegmentsAndStableHash(t *testing.T) {
	packets := buildPackets(t)
	opts := Options{
		QueueDepth: 2, MemLimitBytes: 8 << 20,
		TargetSegmentMS: 2000,
	}
	r1, err := Run(context.Background(), opts, packets, nil,
		media.TicksPerSecond*10, int64(len(packets))*188, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Report.Segments == 0 {
		t.Fatal("expected at least one segment")
	}
	r2, err := Run(context.Background(), opts, packets, nil,
		media.TicksPerSecond*10, int64(len(packets))*188, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.ResumeHash != r2.ResumeHash {
		t.Fatalf("resume hash must be stable: %q vs %q", r1.ResumeHash, r2.ResumeHash)
	}
	if r1.Report.BufferPeakBytes > 8<<20 {
		t.Fatalf("buffer peak %d exceeds 8 MiB ceiling", r1.Report.BufferPeakBytes)
	}
}

// TestSlowConsumerAppliesBackpressure verifies a slow consumer still yields a
// complete result: the bounded queue blocks the producer instead of dropping.
func TestSlowConsumerAppliesBackpressure(t *testing.T) {
	packets := buildPackets(t)
	opts := Options{
		QueueDepth: 2, MemLimitBytes: 8 << 20,
		TargetSegmentMS: 2000, SlowConsumerMS: 5,
	}
	start := time.Now()
	r, err := Run(context.Background(), opts, packets, nil,
		media.TicksPerSecond*10, int64(len(packets))*188, nil)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 5*time.Millisecond {
		t.Fatal("slow-consumer delay was not observed")
	}
	if r.Report.Segments == 0 {
		t.Fatal("backpressure must not lose segments")
	}
}

// TestCancellationStopsStages cancels mid-run and expects an ErrCancelled
// classification rather than a completed result.
func TestCancellationStopsStages(t *testing.T) {
	packets := buildPackets(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()
	opts := Options{
		QueueDepth: 1, MemLimitBytes: 8 << 20,
		TargetSegmentMS: 2000, SlowConsumerMS: 50,
	}
	_, err := Run(ctx, opts, packets, nil,
		media.TicksPerSecond*10, int64(len(packets))*188, nil)
	if err == nil {
		t.Fatal("cancelled run must return an error")
	}
	if media.CodeOf(err) != media.ECancelled {
		t.Fatalf("want ECancelled, got %v", err)
	}
}

// TestCheckpointPreventsRepublish proves a resumed run carrying a checkpoint
// of already-published ids marks those entries non-publishable (no dup emit).
func TestCheckpointPreventsRepublish(t *testing.T) {
	packets := buildPackets(t)
	opts := Options{
		QueueDepth: 2, MemLimitBytes: 8 << 20, TargetSegmentMS: 2000,
	}
	first, err := Run(context.Background(), opts, packets, nil,
		media.TicksPerSecond*10, int64(len(packets))*188, nil)
	if err != nil {
		t.Fatal(err)
	}
	cp := NewCheckpoint()
	for _, id := range first.PublishedIDs {
		cp.Published[id] = struct{}{}
	}
	second, err := Run(context.Background(), opts, packets, nil,
		media.TicksPerSecond*10, int64(len(packets))*188, cp)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.PublishedIDs) != 0 {
		t.Fatalf("resumed run must republish nothing, got %d", len(second.PublishedIDs))
	}
}

// TestMemLimitEnforced confirms a tiny ceiling surfaces ErrResourceLimit
// rather than growing without bound.
func TestMemLimitEnforced(t *testing.T) {
	packets := buildPackets(t)
	opts := Options{
		QueueDepth: 2, MemLimitBytes: 64, TargetSegmentMS: 2000,
	}
	_, err := Run(context.Background(), opts, packets, nil,
		media.TicksPerSecond*10, int64(len(packets))*188, nil)
	if err == nil || media.CodeOf(err) != media.EResourceLimit {
		t.Fatalf("want ErrResourceLimit, got %v", err)
	}
}
