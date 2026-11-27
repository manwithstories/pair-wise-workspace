package flow

import (
	"context"
	"errors"
	"testing"
	"time"

	"relay/internal/media"
)

func newTestPipe(cap int64) *Pipe[*media.Frame] {
	return NewPipe[*media.Frame](cap, FrameCost)
}

func sizedFrame(seq uint64, payload int) *media.Frame {
	return &media.Frame{
		Version: media.WireVersion1, Sequence: seq,
		Payload: make([]byte, payload),
	}
}

func TestSendRecvAck(t *testing.T) {
	p := newTestPipe(1 << 20)
	ctx := context.Background()
	if err := p.Send(ctx, sizedFrame(1, 100)); err != nil {
		t.Fatal(err)
	}
	f, ok, err := p.Recv(ctx)
	if err != nil || !ok || f.Sequence != 1 {
		t.Fatalf("recv: %v ok=%v %v", err, ok, f)
	}
	if p.UsedBytes() == 0 {
		t.Fatal("recv must not release credits before ack")
	}
	p.Ack(f)
	if p.UsedBytes() != 0 {
		t.Fatalf("ack did not release credits: %d", p.UsedBytes())
	}
}

func TestPeakNeverExceedsCapacity(t *testing.T) {
	const cap = 4096
	p := newTestPipe(cap)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 200; i++ {
			if err := p.Send(ctx, sizedFrame(uint64(i), 500)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	// 慢消费者：逐帧处理并确认。
	for n := 0; n < 200; n++ {
		f, ok, err := p.Recv(ctx)
		if err != nil || !ok {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
		p.Ack(f)
		if p.PeakBytes() > cap {
			t.Fatalf("peak %d exceeds capacity %d", p.PeakBytes(), cap)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("producer: %v", err)
	}
}

func TestSlowConsumerBackpressuresProducer(t *testing.T) {
	// 容量仅够容纳两帧，第三帧必须阻塞直到消费者确认。
	p := newTestPipe(2 * (100 + media.HeaderLen + media.TailLen))
	ctx := context.Background()
	if err := p.Send(ctx, sizedFrame(1, 100)); err != nil {
		t.Fatal(err)
	}
	if err := p.Send(ctx, sizedFrame(2, 100)); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() {
		blocked <- p.Send(ctx, sizedFrame(3, 100))
	}()
	select {
	case err := <-blocked:
		t.Fatalf("expected backpressure, send returned early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	f, _, _ := p.Recv(ctx)
	p.Ack(f)
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("send after ack: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer stuck after consumer released credit")
	}
}

func TestCancelReleasesBlockedSend(t *testing.T) {
	p := newTestPipe(100 + media.HeaderLen + media.TailLen)
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.Send(ctx, sizedFrame(1, 100)); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- p.Send(ctx, sizedFrame(2, 100)) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock producer")
	}
}

func TestCloseDrainsAndResetsCredits(t *testing.T) {
	p := newTestPipe(1 << 20)
	ctx := context.Background()
	_ = p.Send(ctx, sizedFrame(1, 10))
	_ = p.Send(ctx, sizedFrame(2, 10))
	p.Close()
	if p.UsedBytes() != 0 {
		t.Fatalf("close must release all credits, used=%d", p.UsedBytes())
	}
	// 关闭后已排队帧仍可被排空，排空后 Recv 返回 ok=false。
	seen := 0
	for {
		f, ok, err := p.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		_ = f
		seen++
	}
	if seen != 2 {
		t.Fatalf("drained %d frames, want 2", seen)
	}
}
