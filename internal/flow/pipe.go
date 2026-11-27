// Package flow 提供以字节额度为硬边界的有界帧管道：
// 慢消费者形成背压，不丢帧、不无限缓存；取消时释放全部在途额度。
package flow

import (
	"context"
	"sync"

	"relay/internal/media"
)

// defaultQueueDepth 是帧条目数的固定上限，字节额度才是主要边界。
const defaultQueueDepth = 4096

// Pipe 是固定容量的有界管道。used 含队列中与消费者已取出但未确认的帧。
type Pipe struct {
	capacity int64

	mu   sync.Mutex
	cond *sync.Cond
	used int64
	peak int64

	queue  chan *media.Frame
	closed bool
}

// NewPipe 创建容量为 capacityBytes 的管道；容量即允许的峰值缓冲硬上限。
func NewPipe(capacityBytes int64) *Pipe {
	p := &Pipe{capacity: capacityBytes, queue: make(chan *media.Frame, defaultQueueDepth)}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// frameCost 计算一帧占用的字节额度（载荷 + 固定记录开销）。
func frameCost(f *media.Frame) int64 {
	return int64(len(f.Payload)) + int64(media.HeaderLen+media.TailLen)
}

// Acquire 阻塞直到取得 n 字节额度或 ctx 取消；取消不改变已用额度。
func (p *Pipe) Acquire(ctx context.Context, n int64) error {
	if n > p.capacity {
		return media.NewFault(media.StageLength, 0, 0, 0,
			"frame cost %d exceeds pipe capacity %d", n, p.capacity)
	}
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.cond.Broadcast()
		p.mu.Unlock()
	})
	defer stop()

	p.mu.Lock()
	defer p.mu.Unlock()
	for p.used+n > p.capacity {
		// 监听取消：ctx 结束后唤醒并退出等待，形成可中断的背压。
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		p.cond.Wait()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	p.used += n
	if p.used > p.peak {
		p.peak = p.used
	}
	return nil
}

// Send 取得额度并入队；队列满或额度不足时阻塞，天然形成背压。
func (p *Pipe) Send(ctx context.Context, f *media.Frame) error {
	cost := frameCost(f)
	if err := p.Acquire(ctx, cost); err != nil {
		return err
	}
	select {
	case p.queue <- f:
		return nil
	case <-ctx.Done():
		p.Release(f) // 入队失败，立即归还额度
		return ctx.Err()
	}
}

// Recv 阻塞取下一帧；管道关闭且排空后返回 (nil, nil)。
// 取出不释放额度——消费者提交输出后须调用 Ack 归还。
func (p *Pipe) Recv(ctx context.Context) (*media.Frame, error) {
	select {
	case f, ok := <-p.queue:
		if !ok {
			return nil, nil
		}
		return f, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Ack 归还一帧占用的额度并唤醒等待中的生产者。
func (p *Pipe) Ack(f *media.Frame) {
	p.mu.Lock()
	p.used -= frameCost(f)
	if p.used < 0 {
		p.used = 0
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

// Release 是 Ack 的语义别名，用于取消路径上的额度释放。
func (p *Pipe) Release(f *media.Frame) { p.Ack(f) }

// Close 关闭入队通道；关闭前排空队列并释放全部在途额度，
// 保证取消/中断路径上没有额度泄漏。
func (p *Pipe) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.queue)
	p.used = 0
	p.cond.Broadcast()
	p.mu.Unlock()
}

// PeakBytes 返回生命周期内观测到的在途字节峰值。
func (p *Pipe) PeakBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}

// UsedBytes 返回当前在途字节。
func (p *Pipe) UsedBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.used
}

// Capacity 返回硬容量。
func (p *Pipe) Capacity() int64 { return p.capacity }
