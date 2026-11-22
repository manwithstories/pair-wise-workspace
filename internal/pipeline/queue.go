// Package pipeline 把 RTP 重排、WAV 对齐、码流检查与恢复串成有界管线，
// 提供背压、缺块恢复与跨阶段不变量。
package pipeline

import (
	"errors"
	"time"
)

// ErrQueueFull 在非阻塞 TryPush 队列满时返回。
var ErrQueueFull = errors.New("pipeline: bounded queue full")

// Block 是队列中流转的通用媒体块：携带序号、时间刻度范围与原始字节。
type Block struct {
	Seq       uint16
	TS        uint32 // RTP 时间戳（32 位）
	SampleLo  uint64 // 统一采样刻度上的起点
	SampleHi  uint64 // 统一采样刻度上的终点（闭开）
	Payload   []byte
	// Redundant 标记该块来自冗余通道，可用于恢复校验。
	Redundant bool
	// Checksum 是恢复校验用的简单 FNV-1a 摘要。
	Checksum uint32
}

// BoundedQueue 是容量严格受限的阻塞队列。容量由参数固定，
// 不会因为突发流量无限增长——满时 Push 阻塞并累计背压时长。
type BoundedQueue struct {
	capacity int
	ch       chan *Block

	enqueued int64
	dequeued int64
	peak     int
	// 背压统计（纳秒）与背压次数。
	backpressureNS int64
	backpressureEvents int
}

// NewBoundedQueue 构造容量为 capacity（>=1）的有界队列。
func NewBoundedQueue(capacity int) (*BoundedQueue, error) {
	if capacity <= 0 {
		return nil, errors.New("pipeline: queue capacity must be positive")
	}
	return &BoundedQueue{
		capacity: capacity,
		ch:       make(chan *Block, capacity),
	}, nil
}

// Push 阻塞式入队。队列满时阻塞直到有空位，并把等待计入背压时长。
func (q *BoundedQueue) Push(b *Block) {
	for {
		select {
		case q.ch <- b:
			q.afterEnqueue()
			return
		default:
		}
		// 满：记录背压后阻塞等待。
		start := time.Now()
		q.backpressureEvents++
		q.ch <- b
		q.backpressureNS += time.Since(start).Nanoseconds()
		q.afterEnqueue()
		return
	}
}

// TryPush 非阻塞入队；满时返回 ErrQueueFull 且不改变队列。
func (q *BoundedQueue) TryPush(b *Block) error {
	select {
	case q.ch <- b:
		q.afterEnqueue()
		return nil
	default:
		return ErrQueueFull
	}
}

func (q *BoundedQueue) afterEnqueue() {
	q.enqueued++
	if n := q.Len(); n > q.peak {
		q.peak = n
	}
}

// Pop 阻塞式出队。
func (q *BoundedQueue) Pop() *Block {
	b := <-q.ch
	q.dequeued++
	return b
}

// TryPop 非阻塞出队；空时返回 nil。
func (q *BoundedQueue) TryPop() *Block {
	select {
	case b := <-q.ch:
		q.dequeued++
		return b
	default:
		return nil
	}
}

// Len 返回当前排队块数。
func (q *BoundedQueue) Len() int { return len(q.ch) }

// Cap 返回固定容量。
func (q *BoundedQueue) Cap() int { return q.capacity }

// Peak 返回排队块数的历史高水位。
func (q *BoundedQueue) Peak() int { return q.peak }

// BackpressureMS 返回累计背压时长（毫秒，四舍五入）。
func (q *BoundedQueue) BackpressureMS() int64 {
	return (q.backpressureNS + 500_000) / 1_000_000
}

// BackpressureEvents 返回触发背压的次数。
func (q *BoundedQueue) BackpressureEvents() int { return q.backpressureEvents }

// Enqueued / Dequeued 返回累计出入队计数。
func (q *BoundedQueue) Enqueued() int64 { return q.enqueued }
func (q *BoundedQueue) Dequeued() int64 { return q.dequeued }
