package stream

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestBackpressureBlocksProducer(t *testing.T) {
	q := NewQueue[int](2)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := q.Push(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		// 缓冲已满：第三次 Push 必须阻塞，直到消费者取走元素。
		if err := q.Push(ctx, 3); err != nil {
			t.Errorf("push: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("缓冲满时 Push 不应立即返回")
	case <-time.After(30 * time.Millisecond):
	}
	if _, ok := q.Pop(ctx); !ok {
		t.Fatal("pop 失败")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("消费后 Push 应解除阻塞")
	}
	st := q.Stats()
	if st.PeakLen > st.Capacity {
		t.Fatalf("峰值 %d 超过容量 %d", st.PeakLen, st.Capacity)
	}
	if st.BackpressureW <= 0 {
		t.Fatalf("应记录背压等待，实际 %v", st.BackpressureW)
	}
}

func TestCancelUnblocksPushAndPop(t *testing.T) {
	q := NewQueue[int](1)
	_ = q.Push(context.Background(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	pushDone := make(chan error, 1)
	go func() { pushDone <- q.Push(ctx, 2) }()
	popDone := make(chan struct{})
	go func() {
		q2 := NewQueue[int](1)
		_, _ = q2.Pop(ctx)
		close(popDone)
	}()
	cancel()
	select {
	case err := <-pushDone:
		if err == nil {
			t.Fatal("取消后 Push 应返回错误")
		}
	case <-time.After(time.Second):
		t.Fatal("取消未能解除 Push 阻塞")
	}
	select {
	case <-popDone:
	case <-time.After(time.Second):
		t.Fatal("取消未能解除 Pop 阻塞")
	}
}

func TestCloseDrainsBuffered(t *testing.T) {
	q := NewQueue[int](4)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = q.Push(ctx, i)
	}
	q.Close()
	got := 0
	for {
		_, ok := q.Pop(ctx)
		if !ok {
			break
		}
		got++
	}
	if got != 3 {
		t.Fatalf("关闭后应排空 3 个元素，实际 %d", got)
	}
	if err := q.Push(ctx, 9); err != ErrClosed {
		t.Fatalf("向已关闭队列 Push 应返回 ErrClosed，实际 %v", err)
	}
}

func TestConcurrentProducersPeakBounded(t *testing.T) {
	q := NewQueue[int](3)
	ctx := context.Background()
	var wg sync.WaitGroup
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := q.Push(ctx, i); err != nil {
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		q.Close()
	}()
	n := 0
	for {
		if _, ok := q.Pop(ctx); !ok {
			break
		}
		n++
	}
	if st := q.Stats(); st.PeakLen > st.Capacity {
		t.Fatalf("峰值 %d 超过容量 %d", st.PeakLen, st.Capacity)
	}
}
