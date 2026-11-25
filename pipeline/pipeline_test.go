package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"fieldpost/internal/core"
)

func waitForEvent(t *testing.T, g *Gate) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if g.Snapshot().BackpressureEvents > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("backpressure event not recorded")
}

func TestGateBackpressureAndPeak(t *testing.T) {
	g := NewGate(4096)
	if err := g.Reserve(context.Background(), 4096); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.Reserve(context.Background(), 1024) }()
	waitForEvent(t, g)
	if got := g.Snapshot(); got.PeakInFlight > 4096 || got.BackpressureEvents != 1 {
		t.Fatalf("snapshot=%+v", got)
	}
	g.Release(2048)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reserve did not wake after release")
	}
	if got := g.Snapshot(); got.InFlight != 3072 || got.PeakInFlight != 4096 {
		t.Fatalf("after wake snapshot=%+v", got)
	}
	g.Release(3072)
}

func TestPipelineAppliesBackpressureAndDrains(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	p := New(context.Background(), 4096, 1, 1, func(_ context.Context, item Item) error {
		if item.ID == "blocking" {
			started <- struct{}{}
			<-release
		}
		return nil
	})
	if err := p.Submit(context.Background(), Item{ID: "blocking", Weight: 2048}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.Submit(context.Background(), Item{ID: "queued", Weight: 2048}); err != nil {
		t.Fatal(err)
	}
	submitted := make(chan error, 1)
	go func() {
		submitted <- p.Submit(context.Background(), Item{ID: "pressured", Weight: 2048})
	}()
	waitForEvent(t, p.gate)
	close(release)
	select {
	case err := <-submitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pressured submit did not complete")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	s := p.Snapshot()
	if s.BackpressureEvents == 0 || s.PeakInFlight > 4096 || s.InFlight != 0 {
		t.Fatalf("snapshot=%+v", s)
	}
	if s.Processed != 3 {
		t.Fatalf("processed=%d", s.Processed)
	}
}

func TestPipelineHandlerFailurePropagates(t *testing.T) {
	want := core.InvariantError(core.StageRemux, "out", 0, "write failed")
	p := New(context.Background(), 4096, 1, 1, func(context.Context, Item) error {
		return want
	})
	if err := p.Submit(context.Background(), Item{ID: "bad", Weight: 16}); err != nil {
		t.Fatal(err)
	}
	err := p.Close()
	if !errors.Is(err, want) && core.ExitCode(err) != core.ExitInvariant {
		t.Fatalf("close error=%v", err)
	}
	if p.Snapshot().Failed != 1 {
		t.Fatalf("snapshot=%+v", p.Snapshot())
	}
}

func TestPipelineCancelUnblocksReserveAndReclaimsWorkers(t *testing.T) {
	g := NewGate(128)
	if err := g.Reserve(context.Background(), 128); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Reserve(ctx, 64) }()
	waitForEvent(t, g)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock reserve")
	}
	g.Release(128)

	block := make(chan struct{})
	p := New(context.Background(), 128, 1, 0, func(ctx context.Context, _ Item) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return ctx.Err()
	})
	if err := p.Submit(context.Background(), Item{ID: "wait", Weight: 32}); err != nil {
		t.Fatal(err)
	}
	close(block)
	if err := p.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("close=%v", err)
	}
	if s := p.Snapshot(); s.InFlight != 0 {
		t.Fatalf("inflight=%d", s.InFlight)
	}
}
