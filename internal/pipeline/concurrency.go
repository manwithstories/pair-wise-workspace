package pipeline

import "context"

// MaxConcurrentTasks is the process-wide ceiling for local remux/verify jobs.
const MaxConcurrentTasks = 4

// TaskLimiter is a bounded semaphore. The zero value is not usable; construct
// one with NewTaskLimiter.
type TaskLimiter chan struct{}

func NewTaskLimiter(limit int) TaskLimiter {
	if limit <= 0 {
		limit = 1
	}
	if limit > MaxConcurrentTasks {
		limit = MaxConcurrentTasks
	}
	return make(TaskLimiter, limit)
}

func (l TaskLimiter) TryAcquire() bool {
	select {
	case l <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l TaskLimiter) Acquire(ctx context.Context) error {
	select {
	case l <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l TaskLimiter) Release() {
	<-l
}
