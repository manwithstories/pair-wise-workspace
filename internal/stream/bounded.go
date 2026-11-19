// Package stream 提供固定容量的有界队列：慢消费者触发背压而非无限堆积，
// 取消时通过 context 及时释放阻塞的读写。
package stream

import (
	"context"
	"sync"
	"time"
)

// Stats 记录队列运行统计。
type Stats struct {
	Capacity      int
	PeakLen       int           // 缓冲峰值（样本数）
	Pushed        uint64
	Popped        uint64
	BackpressureW time.Duration // 生产者因满缓冲累计等待时间
	ConsumerWait  time.Duration // 消费者因空缓冲累计等待时间
}

// Queue 是固定容量的泛型阻塞队列。底层 channel 永不关闭，
// 关闭语义由 done + closed 标志实现，避免向关闭 channel 发送导致 panic。
type Queue[T any] struct {
	ch     chan T
	done   chan struct{}
	mu     sync.Mutex
	stats  Stats
	closed bool
}

// NewQueue 创建容量为 capacity 的队列，capacity 必须 >= 1。
func NewQueue[T any](capacity int) *Queue[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &Queue[T]{ch: make(chan T, capacity), done: make(chan struct{}), stats: Stats{Capacity: capacity}}
}

// Push 写入一个元素；缓冲满时阻塞等待（背压），ctx 取消或队列关闭时返回错误。
func (q *Queue[T]) Push(ctx context.Context, v T) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return ErrClosed
	}
	q.mu.Unlock()

	start := time.Now()
	select {
	case q.ch <- v:
		q.recordPush(0)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-q.done:
		return ErrClosed
	default:
	}
	// 快速路径未命中：缓冲已满，进入阻塞等待并计入背压时长。
	select {
	case q.ch <- v:
		q.recordPush(time.Since(start))
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-q.done:
		return ErrClosed
	}
}

func (q *Queue[T]) recordPush(wait time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stats.BackpressureW += wait
	q.stats.Pushed++
	if n := len(q.ch); n > q.stats.PeakLen {
		q.stats.PeakLen = n
	}
}

// Pop 读取一个元素；队列关闭且排空后返回 ok=false。
func (q *Queue[T]) Pop(ctx context.Context) (T, bool) {
	var zero T
	start := time.Now()
	select {
	case v := <-q.ch:
		q.recordPop(0)
		return v, true
	case <-ctx.Done():
		return zero, false
	case <-q.done:
		return q.drain()
	default:
	}
	select {
	case v := <-q.ch:
		q.recordPop(time.Since(start))
		return v, true
	case <-ctx.Done():
		return zero, false
	case <-q.done:
		return q.drain()
	}
}

// drain 在关闭后排空剩余元素，排空后返回 ok=false。
func (q *Queue[T]) drain() (T, bool) {
	var zero T
	for {
		select {
		case v := <-q.ch:
			q.recordPop(0)
			return v, true
		default:
			return zero, false
		}
	}
}

func (q *Queue[T]) recordPop(wait time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stats.ConsumerWait += wait
	q.stats.Popped++
}

// Close 关闭队列，唤醒所有阻塞的读写方；已缓冲元素仍可被消费。幂等。
func (q *Queue[T]) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.done)
	}
}

// Stats 返回当前统计快照。
func (q *Queue[T]) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stats
}

// ErrClosed 表示向已关闭队列写入。
var ErrClosed = errClosed{}

type errClosed struct{}

func (errClosed) Error() string { return "stream: 队列已关闭" }
