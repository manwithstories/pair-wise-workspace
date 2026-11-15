// Package scheduler dispatches independent segment jobs with bounded concurrency.
package scheduler

import (
	"context"
	"time"
)

type Task[T any] struct {
	Index int
	Run   func(context.Context) (T, error)
}

type Outcome[T any] struct {
	Index    int
	Value    T
	Err      error
	Attempts int
}

// Run executes tasks with maxAttempts total tries and exponential backoff.
// A failed task never stops the remaining queue.
func Run[T any](ctx context.Context, workers, maxAttempts int, backoff time.Duration, tasks []Task[T]) []Outcome[T] {
	if workers < 1 {
		workers = 1
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	jobs := make(chan Task[T])
	results := make(chan Outcome[T], len(tasks))
	for w := 0; w < workers; w++ {
		go func() {
			for task := range jobs {
				var out Outcome[T]
				for attempt := 1; attempt <= maxAttempts; attempt++ {
					out.Index, out.Attempts = task.Index, attempt
					out.Value, out.Err = task.Run(ctx)
					if out.Err == nil || attempt == maxAttempts || ctx.Err() != nil {
						break
					}
					delay := backoff
					for i := 1; i < attempt; i++ {
						delay *= 2
						if delay > 5*time.Second {
							delay = 5 * time.Second
							break
						}
					}
					timer := time.NewTimer(delay)
					select {
					case <-ctx.Done():
						timer.Stop()
					case <-timer.C:
					}
				}
				results <- out
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, task := range tasks {
			jobs <- task
		}
	}()
	out := make([]Outcome[T], 0, len(tasks))
	for i := 0; i < len(tasks); i++ {
		out = append(out, <-results)
	}
	return out
}
