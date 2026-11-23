// Package pipeline 以有界通道连接各处理阶段，传播背压与取消信号，
// 并对队列长度、重排窗口与在途 PCM 总量实施硬上限。
package pipeline

import (
	"context"
	"sync"

	"media-pipeline/internal/fault"
)

// Stream 是一个容量固定的泛型有界通道。
// 消费者变慢时发送方阻塞，从而把背压一路传播到生产者。
type Stream[T any] struct {
	ch     chan T
	name   string
	cap    int
	closed bool
	mu     sync.Mutex
}

// NewStream 创建容量为 cap（必须 > 0）的有界流。
func NewStream[T any](name string, cap int) *Stream[T] {
	if cap <= 0 {
		cap = 1
	}
	return &Stream[T]{ch: make(chan T, cap), name: name, cap: cap}
}

// Send 在上下文取消或下游失败时返回错误；否则在队列满时阻塞形成背压。
func (s *Stream[T]) Send(ctx context.Context, v T) error {
	select {
	case s.ch <- v:
		return nil
	case <-ctx.Done():
		return fault.New(fault.KindCancelled, s.name, -1, -1, true,
			"send cancelled: "+ctx.Err().Error()).WithOp("pipeline.send").
			Wrap(ctx.Err())
	}
}

// Recv 取下一个元素；ok 为 false 表示流已正常关闭且排空。
func (s *Stream[T]) Recv(ctx context.Context) (T, bool, error) {
	var zero T
	select {
	case v, ok := <-s.ch:
		if !ok {
			return zero, false, nil
		}
		return v, true, nil
	case <-ctx.Done():
		return zero, false, fault.New(fault.KindCancelled, s.name, -1, -1, true,
			"recv cancelled: "+ctx.Err().Error()).WithOp("pipeline.recv").
			Wrap(ctx.Err())
	}
}

// Close 关闭流，通知消费者排空后退出。
func (s *Stream[T]) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		close(s.ch)
		s.closed = true
	}
}

// Cap 返回队列容量，Len 返回当前积压元素数。
func (s *Stream[T]) Cap() int { return s.cap }
func (s *Stream[T]) Len() int { return len(s.ch) }

// Name 返回流名称，用于日志与检查点。
func (s *Stream[T]) Name() string { return s.name }
