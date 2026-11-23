package pipeline

import (
	"context"
	"sync"
)

// StageFunc 是一个处理阶段：在独立 goroutine 中运行，
// 通过有界流收发数据。返回错误时整体管线取消并向上游传播。
type StageFunc func(ctx context.Context) error

// Runner 编排多个并发阶段：任一阶段失败即取消全部阶段，
// 全部成功才返回 nil；错误在所有阶段停止后聚合返回。
type Runner struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	errs   []error
}

// NewRunner 基于父上下文创建可整体取消的编排器。
func NewRunner(parent context.Context) (*Runner, context.Context) {
	ctx, cancel := context.WithCancel(parent)
	return &Runner{cancel: cancel}, ctx
}

// Go 启动一个阶段。阶段中的 panic 会被捕获为错误，避免泄漏 goroutine。
func (r *Runner) Go(ctx context.Context, name string, fn StageFunc) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if err := fn(ctx); err != nil {
			r.mu.Lock()
			r.errs = append(r.errs, stageError{name: name, err: err})
			r.mu.Unlock()
			r.cancel() // 下游失败/取消必须停止上游
		}
	}()
}

// Wait 等待所有阶段退出；返回首个记录的错误（确定性顺序）。
func (r *Runner) Wait() error {
	r.wg.Wait()
	r.cancel()
	if len(r.errs) == 0 {
		return nil
	}
	return r.errs[0]
}

// Cancel 主动取消全部阶段（例如收到中断信号）。
func (r *Runner) Cancel() { r.cancel() }

type stageError struct {
	name string
	err  error
}

func (e stageError) Error() string { return "stage " + e.name + ": " + e.err.Error() }
func (e stageError) Unwrap() error { return e.err }
