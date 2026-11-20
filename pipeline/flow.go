// Package pipeline 串联解析、变换、同步与验证阶段，
// 以有界队列和取消信号处理背压。
package pipeline

import (
	"context"
	"errors"
	"sync"
)

// Stage 是流水线阶段：从 in 读、向 out 写，遵守 ctx 取消。
// 阶段返回错误将取消整条流水线。
type Stage func(ctx context.Context, in <-chan any, out chan<- any) error

// Run 串联多个阶段，队列容量可配置；慢消费者在容量耗尽时形成背压。
// 任一阶段出错或 ctx 取消时，所有阶段及时退出，不遗留阻塞协程。
// 末级输出由 Run 排空。全部成功返回 nil；外部取消返回 ctx.Err()。
func Run(ctx context.Context, queueCap int, stages ...Stage) error {
	if queueCap < 1 {
		queueCap = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	chans := make([]chan any, len(stages)+1)
	chans[0] = make(chan any)
	close(chans[0]) // 首阶段无上游输入
	for i := range stages {
		chans[i+1] = make(chan any, queueCap)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(stages))
	for i, st := range stages {
		wg.Add(1)
		go func(i int, st Stage) {
			defer wg.Done()
			defer close(chans[i+1])
			if err := st(ctx, chans[i], chans[i+1]); err != nil {
				errs[i] = err
				cancel()
			}
		}(i, st)
	}
	// 排空末级输出，防止末阶段阻塞
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for range chans[len(stages)] {
		}
	}()
	wg.Wait()
	<-drainDone

	for _, e := range errs {
		if e != nil && !errors.Is(e, context.Canceled) && !errors.Is(e, context.DeadlineExceeded) {
			return e
		}
	}
	return ctx.Err()
}

// Send 向 out 写入一个值，响应取消；返回 false 表示已取消。
func Send(ctx context.Context, out chan<- any, v any) bool {
	select {
	case out <- v:
		return true
	case <-ctx.Done():
		return false
	}
}

// Recv 从 in 读取一个值；ok=false 表示输入关闭或已取消。
func Recv(ctx context.Context, in <-chan any) (v any, ok bool) {
	select {
	case v, ok := <-in:
		return v, ok
	case <-ctx.Done():
		return nil, false
	}
}
