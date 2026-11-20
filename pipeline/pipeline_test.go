package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"mediamux/media"
)

// source 发射 n 个整数。
func source(n int) Stage {
	return func(ctx context.Context, in <-chan any, out chan<- any) error {
		for i := 0; i < n; i++ {
			if !Send(ctx, out, i) {
				return ctx.Err()
			}
		}
		return nil
	}
}

// doubler 把整数乘 2。
func doubler(ctx context.Context, in <-chan any, out chan<- any) error {
	for {
		v, ok := Recv(ctx, in)
		if !ok {
			return ctx.Err()
		}
		if v == nil {
			return nil
		}
		if !Send(ctx, out, v.(int)*2) {
			return ctx.Err()
		}
	}
}

func TestRunPassThrough(t *testing.T) {
	var got []int
	sink := func(ctx context.Context, in <-chan any, out chan<- any) error {
		for {
			v, ok := Recv(ctx, in)
			if !ok {
				return ctx.Err()
			}
			if v == nil {
				return nil
			}
			got = append(got, v.(int))
		}
	}
	if err := Run(context.Background(), 4, source(10), doubler, sink); err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 || got[9] != 18 {
		t.Fatalf("got = %v", got)
	}
}

func TestRunSlowConsumerBackpressure(t *testing.T) {
	// 慢消费者 + 小队列：必须背压而非丢弃，且最终完成
	slow := func(ctx context.Context, in <-chan any, out chan<- any) error {
		n := 0
		for {
			v, ok := Recv(ctx, in)
			if !ok {
				return ctx.Err()
			}
			if v == nil {
				return nil
			}
			n++
			time.Sleep(2 * time.Millisecond)
		}
	}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), 2, source(50), doubler, slow) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pipeline stuck under backpressure")
	}
}

func TestRunErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	fail := func(ctx context.Context, in <-chan any, out chan<- any) error {
		v, ok := Recv(ctx, in)
		if !ok || v == nil {
			return nil
		}
		return boom
	}
	done := make(chan error, 1)
	// 源端无限发射：故障后必须及时退出，不遗留阻塞协程
	inf := func(ctx context.Context, in <-chan any, out chan<- any) error {
		for i := 0; ; i++ {
			if !Send(ctx, out, i) {
				return ctx.Err()
			}
		}
	}
	go func() { done <- Run(context.Background(), 2, inf, fail) }()
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline did not exit after stage failure")
	}
}

func TestRunCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inf := func(ctx context.Context, in <-chan any, out chan<- any) error {
		for i := 0; ; i++ {
			if !Send(ctx, out, i) {
				return ctx.Err()
			}
		}
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, 2, inf, doubler) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline did not exit after cancel")
	}
}

func TestAligner(t *testing.T) {
	a := NewAligner()
	// 参考轨每 40ms 一个点（25fps）
	for i := 0; i < 100; i++ {
		a.ObserveReference(media.MillisToTicks(int64(i * 40)))
	}
	// 字幕偏移 +10ms
	for i := 0; i < 10; i++ {
		a.ObserveSubtitle(media.MillisToTicks(int64(i*400 + 10)))
	}
	rep := a.Report()
	if rep.Incomplete || rep.Count != 10 {
		t.Fatalf("report = %+v", rep)
	}
	if media.Millis(rep.P95) != 10 || media.Millis(rep.MaxErr) != 10 || media.Millis(rep.AvgErr) != 10 {
		t.Fatalf("errs: p95=%d max=%d avg=%d",
			media.Millis(rep.P95), media.Millis(rep.MaxErr), media.Millis(rep.AvgErr))
	}
}

func TestAlignerIncomplete(t *testing.T) {
	// 缺少参考轨
	a := NewAligner()
	a.ObserveSubtitle(media.MillisToTicks(1000))
	if rep := a.Report(); !rep.Incomplete || len(rep.Reasons) == 0 {
		t.Fatalf("no-ref report = %+v", rep)
	}
	// 时间戳倒退
	b := NewAligner()
	b.ObserveReference(media.MillisToTicks(2000))
	b.ObserveReference(media.MillisToTicks(1000))
	b.ObserveSubtitle(media.MillisToTicks(1500))
	if rep := b.Report(); !rep.Incomplete {
		t.Fatalf("regress report = %+v", rep)
	}
	// 无字幕
	c := NewAligner()
	c.ObserveReference(media.MillisToTicks(0))
	if rep := c.Report(); !rep.Incomplete {
		t.Fatalf("empty report = %+v", rep)
	}
}
