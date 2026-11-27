package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"mediacheck/internal/ingest"
	"mediacheck/internal/model"
)

func fixture(t *testing.T, frames int, profile ingest.Profile) *ingest.Generated {
	t.Helper()
	cfg := ingest.DefaultConfig(17)
	cfg.VideoFrames = frames
	g, err := ingest.Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := g.ApplyProfile(profile); err != nil {
		t.Fatalf("ApplyProfile: %v", err)
	}
	return g
}

func TestRunCleanFixture(t *testing.T) {
	g := fixture(t, 100, ingest.ProfileClean)
	res, err := Run(context.Background(), g, Options{QueueItems: 16, QueueBytes: 1 << 20, Recovery: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Canceled {
		t.Fatal("clean run reported cancellation")
	}
	want := len(g.Audio.Packets) + len(g.Video.Packets)
	if res.Packets != want {
		t.Fatalf("packets = %d, want %d", res.Packets, want)
	}
	if res.Queue.PeakItems > 16 {
		t.Fatalf("queue peak %d exceeded bound 16", res.Queue.PeakItems)
	}
	if res.Recovery.Slots != 0 {
		t.Fatalf("clean run filled %d slots", res.Recovery.Slots)
	}
	if len(res.Faults) != 0 {
		t.Fatalf("clean run faults: %v", res.Faults)
	}
}

func TestRunDamagedFixtureRecoversSlots(t *testing.T) {
	g := fixture(t, 200, ingest.ProfileDamaged)
	res, err := Run(context.Background(), g, Options{QueueItems: 32, QueueBytes: 1 << 20, Recovery: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Recovery.Slots == 0 {
		t.Fatal("no lost slots were filled")
	}
	if res.Recovery.Failed != 0 {
		t.Fatalf("%d slots could not be filled", res.Recovery.Failed)
	}
	if got := res.Recovery.RecoveryRate(); got < 95 {
		t.Fatalf("recovery rate = %.2f%%, want >= 95%%", got)
	}
	if res.Recovery.FEC+res.Recovery.PLC+res.Recovery.Silence != res.Recovery.Slots {
		t.Fatalf("method counts do not add up: %+v", res.Recovery)
	}
	if len(res.Damage.Ranges()) == 0 {
		t.Fatal("damaged run produced no damaged ranges")
	}
}

func TestRunTruncatedFixtureKeepsEveryPacket(t *testing.T) {
	g := fixture(t, 150, ingest.ProfileTruncated)
	res, err := Run(context.Background(), g, Options{QueueItems: 16, QueueBytes: 1 << 20, Recovery: true})
	if err != nil {
		t.Fatal(err)
	}
	want := len(g.Audio.Packets) + len(g.Video.Packets)
	if res.Packets != want {
		t.Fatalf("truncation lost packets: got %d want %d", res.Packets, want)
	}
	if res.Damage.Len() == 0 {
		t.Fatal("truncated run produced no damaged ranges")
	}
}

func TestRunCancelUnblocksFullQueue(t *testing.T) {
	g := fixture(t, 600, ingest.ProfileClean)
	// The consumer stalls on every packet while the queue is tiny, so the
	// reader blocks in Push almost immediately.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var res *Result
	go func() {
		var err error
		res, err = Run(ctx, g, Options{
			QueueItems:   1,
			QueueBytes:   1 << 20,
			SlowConsumer: 200 * time.Millisecond,
			Recovery:     true,
		})
		done <- err
	}()
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, model.ErrCanceled) && !errors.Is(err, model.ErrCanceled) {
			t.Fatalf("Run after cancel = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not unblock the pipeline")
	}
	if res == nil {
		t.Fatal("no result captured")
	}
	if !res.Canceled {
		t.Fatal("result not marked canceled")
	}
	if res.Packets >= len(g.Audio.Packets)+len(g.Video.Packets) {
		t.Fatal("cancelled run processed the whole fixture")
	}
	if res.Queue.PeakItems > 1 {
		t.Fatalf("queue peak %d exceeded the bound", res.Queue.PeakItems)
	}
}

func TestSlowConsumerForcesBackpressure(t *testing.T) {
	g := fixture(t, 60, ingest.ProfileSlow)
	// Without a delay the queue peak is small; with a delay the producer must
	// wait, which is observable as a nonzero peak and a longer run.
	fast, err := Run(context.Background(), g, Options{QueueItems: 4, QueueBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	slow, err := Run(context.Background(), g, Options{QueueItems: 4, QueueBytes: 1 << 20, SlowConsumer: 200 * time.Microsecond})
	if err != nil {
		t.Fatal(err)
	}
	if slow.Queue.PeakItems < fast.Queue.PeakItems {
		t.Fatalf("slow peak %d below fast peak %d", slow.Queue.PeakItems, fast.Queue.PeakItems)
	}
	if slow.Queue.PeakItems > 4 {
		t.Fatalf("peak %d exceeded bound 4", slow.Queue.PeakItems)
	}
	if slow.Latency.Count == 0 || slow.Latency.P95 <= 0 {
		t.Fatalf("latency was not measured: %+v", slow.Latency)
	}
}

func TestLatencyWindowIsBoundedAndStable(t *testing.T) {
	w := newLatencyWindow(100)
	for i := 1; i <= 10000; i++ {
		w.add(time.Duration(i) * time.Microsecond)
	}
	if len(w.values) != 100 {
		t.Fatalf("window kept %d samples, want 100", len(w.values))
	}
	st := w.stats()
	if st.Count != 10000 {
		t.Fatalf("count = %d, want 10000", st.Count)
	}
	if st.P95 < st.P50 || st.Max < st.P95 {
		t.Fatalf("percentiles out of order: %+v", st)
	}
}

func TestSplitGapProducesContiguousSlots(t *testing.T) {
	slots := splitGap(1000, 75, 20)
	if len(slots) != 4 {
		t.Fatalf("slots = %d, want 4", len(slots))
	}
	last := int64(1000)
	for _, s := range slots {
		if s.Start != last || s.End <= s.Start {
			t.Fatalf("slot %+v not contiguous after %d", s, last)
		}
		last = s.End
	}
	if last != 1075 {
		t.Fatalf("gap filled to %d, want 1075", last)
	}
	if splitGap(0, 0, 20) != nil || splitGap(0, 10, 0) != nil {
		t.Fatal("degenerate gaps must not produce slots")
	}
}

func TestRunWrapFixtureReportsWrapWithoutStopping(t *testing.T) {
	g := fixture(t, 150, ingest.ProfileWrap)
	res, err := Run(context.Background(), g, Options{QueueItems: 8, QueueBytes: 1 << 20, Recovery: true})
	if err != nil {
		t.Fatal(err)
	}
	wraps := 0
	for _, f := range res.Faults {
		if f.Kind == model.FaultSequenceWrap {
			wraps++
		}
	}
	if wraps != 1 {
		t.Fatalf("wrap faults = %d, want 1", wraps)
	}
	want := len(g.Audio.Packets) + len(g.Video.Packets)
	if res.Packets != want {
		t.Fatalf("wrap lost packets: got %d want %d", res.Packets, want)
	}
}

func TestRunAACFixtureDoesNotUseOpusRecovery(t *testing.T) {
	cfg := ingest.DefaultConfig(17)
	cfg.VideoFrames = 100
	cfg.Codec = model.CodecAAC
	g, err := ingest.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.ApplyProfile(ingest.ProfileDamaged); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), g, Options{QueueItems: 8, QueueBytes: 1 << 20, Recovery: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Capabilities.FEC || res.Capabilities.PLC {
		t.Fatalf("AAC run claimed Opus recovery capabilities: %+v", res.Capabilities)
	}
	if res.Recovery.Slots == 0 || res.Recovery.Failed == 0 {
		t.Fatalf("AAC losses were not reported as unfilled: %+v", res.Recovery)
	}
	if got := res.Recovery.RecoveryRate(); got != 0 {
		t.Fatalf("AAC recovery rate = %v, want 0", got)
	}
}

func TestConsumerErrorDoesNotDeadlockReader(t *testing.T) {
	g := fixture(t, 300, ingest.ProfileClean)
	wantErr := errors.New("disk full")
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), g, Options{
			QueueItems:   2,
			QueueBytes:   1 << 20,
			SlowConsumer: 50 * time.Microsecond,
			Recovery:     true,
			Emit: func(p model.Packet) error {
				if p.Sequence == 5 {
					return wantErr
				}
				return nil
			},
		})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("Run error = %v, want the consumer error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run deadlocked after the consumer returned an error")
	}
}
