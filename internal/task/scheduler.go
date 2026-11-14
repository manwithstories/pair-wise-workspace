package task

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrSuccessLimit = errors.New("scheduler stopped after success limit")

type Work func(ctx context.Context, attempt int) error

type Task struct {
	ID  int
	Run Work
}

type SchedulerHooks struct {
	OnRetry   func(id, attempt int, err error)
	OnFailure func(id int, err error)
}

type Scheduler struct {
	Concurrency  int
	MaxRetries   int
	BackoffBase  time.Duration
	MaxBackoff   time.Duration
	SuccessLimit int
	Hooks        SchedulerHooks
}

func (s *Scheduler) Run(ctx context.Context, tasks []Task) error {
	if len(tasks) == 0 {
		return nil
	}
	if s.Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1")
	}
	if s.MaxRetries < 0 {
		return fmt.Errorf("max retries must be non-negative")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan Task)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	successes := 0
	stopped := false

	setErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}
	for i := 0; i < s.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-runCtx.Done():
					return
				case t, ok := <-queue:
					if !ok {
						return
					}
					if err := s.runTask(runCtx, t, setErr); err != nil {
						if runCtx.Err() != nil {
							return
						}
						setErr(err)
						return
					}
					mu.Lock()
					successes++
					reached := s.SuccessLimit > 0 && successes >= s.SuccessLimit
					if reached {
						stopped = true
					}
					mu.Unlock()
					if reached {
						cancel()
						return
					}
				}
			}
		}()
	}
feed:
	for _, t := range tasks {
		select {
		case <-runCtx.Done():
			break feed
		case queue <- t:
		}
	}
	close(queue)
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	if stopped {
		return ErrSuccessLimit
	}
	return nil
}

func (s *Scheduler) runTask(ctx context.Context, t Task, fail func(error)) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := t.Run(ctx, attempt)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= s.MaxRetries {
			if s.Hooks.OnFailure != nil {
				s.Hooks.OnFailure(t.ID, err)
			}
			return fmt.Errorf("segment %d failed after %d attempts: %w", t.ID, attempt+1, err)
		}
		if s.Hooks.OnRetry != nil {
			s.Hooks.OnRetry(t.ID, attempt+1, err)
		}
		if err := s.sleepBackoff(ctx, attempt); err != nil {
			return err
		}
	}
}

func (s *Scheduler) sleepBackoff(ctx context.Context, attempt int) error {
	base := s.BackoffBase
	if base <= 0 {
		base = 25 * time.Millisecond
	}
	delay := base
	for i := 0; i < attempt && delay < s.MaxBackoff; i++ {
		delay *= 2
	}
	if s.MaxBackoff > 0 && delay > s.MaxBackoff {
		delay = s.MaxBackoff
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
