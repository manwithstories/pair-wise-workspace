// concurrency.go 提供带容量上限的并发限流器。
//
// 归档流水线为保证片段序号、中断计数与产物哈希的确定性，按素材顺序串行
// 处理；但底层资源（适配器解析、解码）仍受 MaxConcurrency 约束。Semaphore
// 把“同时在途的重活数量”钳制在配置上限内，默认 2，防止资源失控。
package pipeline

import "archivemedia/internal/model"

// Semaphore 是计数信号量。
type Semaphore struct {
	ch chan struct{}
}

// NewSemaphore 构造容量为 n 的限流器；n<=0 视为非法参数。
func NewSemaphore(n int) (*Semaphore, error) {
	if n <= 0 {
		return nil, model.NewError(model.KindInvalidArgument,
			"pipeline: max concurrency must be positive, got %d", n)
	}
	return &Semaphore{ch: make(chan struct{}, n)}, nil
}

// Acquire 占用一个并发槽。
func (s *Semaphore) Acquire() { s.ch <- struct{}{} }

// Release 归还一个并发槽。
func (s *Semaphore) Release() { <-s.ch }

// Cap 返回容量。
func (s *Semaphore) Cap() int { return cap(s.ch) }
