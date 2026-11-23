package pipeline

import (
	"context"
	"fmt"
	"sync"

	"media-pipeline/internal/fault"
)

// Budget 用信号量语义限制在途 PCM 总字节数（默认上限 64 MiB）。
// 申请超限时生产者阻塞，实现全链路内存背压。
type Budget struct {
	tokens chan struct{}
	limit  int
	mu     sync.Mutex
	inUse  int
}

// NewBudget 创建字节预算限流器，limit 为硬上限。
func NewBudget(limit int) *Budget {
	if limit <= 0 {
		limit = 64 * 1024 * 1024
	}
	return &Budget{tokens: make(chan struct{}, limit), limit: limit}
}

// Acquire 申请 n 字节预算；单块超过上限立即报错，
// 其余情况在预算耗尽时阻塞，直到释放或取消。
func (b *Budget) Acquire(ctx context.Context, n int) error {
	if n > b.limit {
		return fault.New(fault.KindOther, "", -1, -1, false,
			fmt.Sprintf("block of %d bytes exceeds hard buffer limit %d", n, b.limit)).
			WithOp("pipeline.budget")
	}
	for i := 0; i < n; i++ {
		select {
		case b.tokens <- struct{}{}:
		case <-ctx.Done():
			// 回滚已申请部分。
			b.release(i)
			return fault.New(fault.KindCancelled, "", -1, -1, true,
				"budget acquire cancelled").WithOp("pipeline.budget").Wrap(ctx.Err())
		}
	}
	b.mu.Lock()
	b.inUse += n
	b.mu.Unlock()
	return nil
}

// Release 归还 n 字节预算。
func (b *Budget) Release(n int) { b.release(n) }

func (b *Budget) release(n int) {
	for i := 0; i < n; i++ {
		select {
		case <-b.tokens:
		default: // 防御性：释放多于持有时不阻塞
			i = n
		}
	}
	b.mu.Lock()
	if n <= b.inUse {
		b.inUse -= n
	} else {
		b.inUse = 0
	}
	b.mu.Unlock()
}

// InUse 返回当前在途字节数，Limit 返回硬上限。
func (b *Budget) InUse() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inUse
}

// Limit 返回预算硬上限。
func (b *Budget) Limit() int { return b.limit }
