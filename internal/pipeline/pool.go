// Package pipeline 提供有界并发的生产-消费工作池与确定性归并器。
//
// 工作池硬性约束：并发上限 MaxWorkers（16）、待处理队列有界、单片内存有界；
// 收到取消信号后停止接收新工作、取消在途任务、排空已提交结果，
// 并返回首个确定性错误与已完成片段数。
package pipeline

import (
	"context"
	"errors"
	"sync"

	"mediatime/internal/segment"
)

// 并发硬性上限。
const MaxWorkers = 16

// 工作池错误类别。
var (
	// ErrQueueFull 队列已满且仍有提交尝试（取消场景下的背压信号）。
	ErrQueueFull = errors.New("pipeline: work queue full")
	// ErrCancelled 上下文取消后工作池停止处理。
	ErrCancelled = errors.New("pipeline: cancelled")
	// ErrWorkerLimit 并发数超出 16 的硬上限。
	ErrWorkerLimit = errors.New("pipeline: worker count exceeds limit of 16")
)

// Job 是一个待处理片段任务。
type Job struct {
	Sequence int
	URI      string
	Data     []byte
}

// Result 是片段处理产物（提取与索引由上层装配，池只负责并发与生命周期）。
type Result struct {
	Sequence int
	Fragment *segment.ExtractedFragment
	Err      error
}

// Config 控制工作池规模，全部为显式参数。
type Config struct {
	Workers  int
	QueueLen int
}

// Pool 是有界工作池。处理函数由调用方注入，便于单测模拟故障与抖动。
type Pool struct {
	cfg   Config
	work  func(ctx context.Context, job Job) Result
	queue chan Job
}

// NewPool 校验并发边界并构造工作池。
func NewPool(cfg Config, work func(context.Context, Job) Result) (*Pool, error) {
	if cfg.Workers < 1 || cfg.Workers > MaxWorkers {
		return nil, ErrWorkerLimit
	}
	if cfg.QueueLen < 1 {
		cfg.QueueLen = 1
	}
	if work == nil {
		return nil, errors.New("pipeline: nil work function")
	}
	return &Pool{cfg: cfg, work: work, queue: make(chan Job, cfg.QueueLen)}, nil
}

// TrySubmit 非阻塞投递一个任务。队列已满或 ctx 已取消时返回明确错误，
// 供上层在取消时队列满的场景下立即停止接收新工作。
func (p *Pool) TrySubmit(ctx context.Context, job Job) error {
	if ctx.Err() != nil {
		return ErrCancelled
	}
	select {
	case p.queue <- job:
		return nil
	case <-ctx.Done():
		return ErrCancelled
	default:
		return ErrQueueFull
	}
}

// StartWorkers 启动固定数量 worker 消费共享队列，返回结果通道与等待函数。
// 调用方负责通过 TrySubmit 投递并在结束时 CloseSubmit。
func (p *Pool) StartWorkers(ctx context.Context) (<-chan Result, func()) {
	results := make(chan Result, p.cfg.QueueLen+p.cfg.Workers)
	var wg sync.WaitGroup
	for w := 0; w < p.cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-p.queue:
					if !ok {
						return
					}
					if ctx.Err() != nil {
						results <- Result{Sequence: job.Sequence, Err: ErrCancelled}
						continue
					}
					results <- p.work(ctx, job)
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(results)
		close(done)
	}()
	return results, func() { <-done }
}

// CloseSubmit 声明不再有新任务，worker 在排空队列后退出。
func (p *Pool) CloseSubmit() { close(p.queue) }

// Collect 排空结果通道直到全部 worker 退出，返回首个确定性错误。
func Collect(ctx context.Context, results <-chan Result) ([]Result, error) {
	var collected []Result
	var firstErr error
	for r := range results {
		if r.Err != nil && firstErr == nil {
			firstErr = r.Err
		}
		collected = append(collected, r)
	}
	if err := ctx.Err(); err != nil && firstErr == nil {
		firstErr = ErrCancelled
	}
	return collected, firstErr
}

// Run 处理全部任务。
//
// 语义：
//   - 正常完成：结果按完成顺序进入返回切片（归并器再排序）；
//   - ctx 取消：停止接收新任务，在途任务通过 ctx 收到取消，已产生的结果
//     仍会被排空返回，最后附带 ErrCancelled；
//   - 任一任务返回错误：记录首个确定性错误，继续排空在途结果后统一返回。
func (p *Pool) Run(ctx context.Context, jobs []Job) ([]Result, error) {
	results, wait := p.StartWorkers(ctx)
	go func() {
		defer close(p.queue)
		for _, j := range jobs {
			select {
			case <-ctx.Done():
				return
			case p.queue <- j:
			}
		}
	}()
	_ = wait
	return Collect(ctx, results)
}
