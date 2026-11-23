package pipeline_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"media-pipeline/internal/fault"
	"media-pipeline/internal/pipeline"
)

func TestBoundedStreamBackpressure(t *testing.T) {
	s := pipeline.NewStream[int]("q", 2)
	ctx := context.Background()
	// 容量 2，第三次发送必须阻塞直到消费者取走数据。
	if err := s.Send(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, 2); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = s.Send(ctx, 3) // 应阻塞
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("send on full queue did not block: backpressure missing")
	case <-time.After(20 * time.Millisecond):
	}
	v, ok, err := s.Recv(ctx)
	if err != nil || !ok || v != 1 {
		t.Fatalf("recv = %d,%v,%v", v, ok, err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("producer did not unblock after consumer drained one item")
	}
}

func TestCancellationStopsSend(t *testing.T) {
	s := pipeline.NewStream[int]("q", 1)
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Send(ctx, 1); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := s.Send(ctx, 2)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCancelled {
		t.Fatalf("send err = %v, want cancellation", err)
	}
}

func TestRunnerDownstreamFailureStopsUpstream(t *testing.T) {
	r, ctx := pipeline.NewRunner(context.Background())
	up := pipeline.NewStream[int]("up", 1)
	downstreamErr := errors.New("disk write failed")

	r.Go(ctx, "producer", func(ctx context.Context) error {
		for i := 0; ; i++ {
			if err := up.Send(ctx, i); err != nil {
				return err // 下游失败经 ctx 取消传到这里
			}
		}
	})
	r.Go(ctx, "consumer", func(ctx context.Context) error {
		if _, _, err := up.Recv(ctx); err != nil {
			return err
		}
		return downstreamErr
	})

	err := r.Wait()
	if !errors.Is(err, downstreamErr) {
		t.Fatalf("wait err = %v, want downstream failure", err)
	}
}

func TestBudgetHardLimitAndRelease(t *testing.T) {
	b := pipeline.NewBudget(1024)
	ctx := context.Background()
	if err := b.Acquire(ctx, 512); err != nil {
		t.Fatal(err)
	}
	if b.InUse() != 512 {
		t.Fatalf("in-use = %d, want 512", b.InUse())
	}
	// 超硬上限的单块立即拒绝，不等待。
	err := b.Acquire(ctx, 2048)
	if err == nil {
		t.Fatal("oversized block should be rejected")
	}
	// 剩余预算不足，申请应阻塞直到取消。
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := b.Acquire(cctx, 1024); err == nil {
		t.Fatal("acquire over budget should block until cancelled")
	}
	b.Release(512)
	if b.InUse() != 0 {
		t.Fatalf("in-use after release = %d, want 0", b.InUse())
	}
	if err := b.Acquire(ctx, 1024); err != nil {
		t.Fatalf("acquire after full release: %v", err)
	}
}
