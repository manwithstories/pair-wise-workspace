// Package pipeline 实现容量受限的解析流水线。
//
// 解析器向有界队列投递 Record，消费者依序交给索引与摘要模块。
// 队列满时发送方阻塞形成背压（绝不无限缓存）；ctx 取消后所有工作协程退出释放。
package pipeline

import (
	"context"
	"sync"

	"relaycheck/internal/model"
)

// Kind 标识一条流水线记录的子系统来源。
type Kind int

const (
	KindManifest Kind = iota
	KindFragment
	KindRTP
)

// Record 是流水线中的一条不可变工作项。
type Record struct {
	Kind     Kind
	Seq      int
	Fragment model.FragmentReport
	RTP      model.RTPReport
	Manifest model.ManifestReport
}

// Stats 汇总流水线运行期计数，供内存与吞吐核对。
type Stats struct {
	mu        sync.Mutex
	Enqueued  int
	Processed int
	Dropped   int
}

func (s *Stats) incEnq() { s.mu.Lock(); s.Enqueued++; s.mu.Unlock() }
func (s *Stats) incProc() { s.mu.Lock(); s.Processed++; s.mu.Unlock() }
func (s *Stats) incDrop() { s.mu.Lock(); s.Dropped++; s.mu.Unlock() }

// Snapshot 返回计数的一致副本。
func (s *Stats) Snapshot() (enq, proc, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Enqueued, s.Processed, s.Dropped
}

// Pipeline 是单生产者/单消费者的有界流水线。
type Pipeline struct {
	ch     chan Record
	wg     sync.WaitGroup
	stats  Stats
	cancel context.CancelFunc
}

// New 创建容量为 queueCapacity 的流水线并启动消费者；handler 依序处理每条记录。
// queueCapacity<=0 时视为 1，保证有界。
func New(parent context.Context, queueCapacity int, handler func(Record)) (*Pipeline, context.Context) {
	if queueCapacity <= 0 {
		queueCapacity = 1
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pipeline{
		ch:     make(chan Record, queueCapacity),
		cancel: cancel,
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case rec, ok := <-p.ch:
				if !ok {
					return
				}
				handler(rec)
				p.stats.incProc()
			}
		}
	}()
	return p, ctx
}

// Submit 投递一条记录。队列满时阻塞直到消费者取走或 ctx 取消。
// ctx 取消后严格返回 context.Canceled，调用方应停止生产。
func (p *Pipeline) Submit(ctx context.Context, rec Record) error {
	// 取消优先：即使队列仍有空槽也拒绝入队，保证取消语义确定。
	if err := ctx.Err(); err != nil {
		p.stats.incDrop()
		return err
	}
	p.stats.incEnq()
	select {
	case <-ctx.Done():
		p.stats.incDrop()
		return ctx.Err()
	case p.ch <- rec:
		return nil
	}
}

// Close 关闭队列并等待消费者把已入队记录处理完毕（优雅排空）。
func (p *Pipeline) Close() {
	close(p.ch)
	p.wg.Wait()
}

// Cancel 立即通知工作协程退出并等待其释放，不保证排空队列。
func (p *Pipeline) Cancel() {
	p.cancel()
	p.wg.Wait()
}

// Stats 返回运行期统计。
func (p *Pipeline) Stats() *Stats { return &p.stats }
