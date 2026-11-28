// Package worker provides a bounded task pool with cancellation that
// reaches producers, consumers and renderers.
package worker

import (
	"context"
	"sync"
)

// MaxWorkers caps concurrency regardless of the requested value.
const MaxWorkers = 8

// Pool runs tasks with a bounded number of goroutines.
type Pool struct{ n int }

// New returns a pool with n workers, clamped to [1, MaxWorkers].
func New(n int) *Pool {
	if n < 1 {
		n = 1
	}
	if n > MaxWorkers {
		n = MaxWorkers
	}
	return &Pool{n: n}
}

// Size reports the worker count.
func (p *Pool) Size() int { return p.n }

// Run executes all tasks, at most p.n concurrently. Cancelling ctx stops
// the producer (no further tasks are handed out) and is observed by every
// running task; Run waits for in-flight tasks to return before reporting
// the first error or ctx.Err().
func (p *Pool) Run(ctx context.Context, tasks []func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for i := 0; i < p.n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if err := tasks[idx](ctx); err != nil {
					select {
					case errCh <- err:
					default:
					}
					cancel()
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		}()
	}
feed:
	for i := range tasks {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
	}
	return ctx.Err()
}
