// Package pipeline provides a bounded concurrency pool and deterministic
// merging of results from the MP4, RTP and metadata subsystems. The pool
// caps both queue depth and worker count, and any consumer failure or
// context cancellation stops production, cancels in-flight work and drains
// the queue so goroutines never leak.
package pipeline

import (
	"context"
	"errors"
	"sync"
)

// Limits configure pool bounds. Both fields must be positive.
type Limits struct {
	MaxQueueLen   int
	MaxConcurrent int
}

// Validate rejects unusable pool configurations.
func (l Limits) Validate() error {
	if l.MaxQueueLen <= 0 {
		return errors.New("pipeline: MaxQueueLen must be positive")
	}
	if l.MaxConcurrent <= 0 {
		return errors.New("pipeline: MaxConcurrent must be positive")
	}
	return nil
}

// Job is one unit of work. Seq establishes the deterministic input order;
// SegmentID groups jobs into checkpointable ranges.
type Job struct {
	Seq       int64
	SegmentID int64
	Work      func(ctx context.Context) error
}

// Pool runs jobs with bounded queue depth and bounded parallelism.
type Pool struct {
	lim Limits
}

// NewPool constructs a validated pool.
func NewPool(lim Limits) (*Pool, error) {
	if err := lim.Validate(); err != nil {
		return nil, err
	}
	return &Pool{lim: lim}, nil
}

// ErrPoolCanceled is returned when the parent context canceled the run.
var ErrPoolCanceled = errors.New("pipeline: run canceled")

// Run executes jobs on at most MaxConcurrent workers. The job channel buffers
// at most MaxQueueLen items, so memory stays bounded regardless of the total
// job count. It blocks until every job finishes, the context is canceled, or
// a job returns an error. On failure/cancellation production stops
// immediately and all in-flight jobs see a canceled context; the first
// observed error is returned.
func (p *Pool) Run(ctx context.Context, jobs []Job) error {
	if err := p.lim.Validate(); err != nil {
		return err
	}
	// A cancelable child context lets a single job failure stop the rest.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobsCh := make(chan Job, p.lim.MaxQueueLen)
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	fail := func(err error) {
		once.Do(func() {
			firstErr = err
			cancel()
		})
	}

	workers := p.lim.MaxConcurrent
	if workers > len(jobs) {
		workers = len(jobs)
	}
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for j := range jobsCh {
				if runCtx.Err() != nil {
					return // stop consuming; producer closes the channel
				}
				if err := j.Work(runCtx); err != nil {
					fail(err)
					return
				}
			}
		}()
	}

	// Producer: feed the bounded channel, honoring cancellation so it never
	// blocks forever and no further jobs are dispatched after a failure.
producer:
	for _, j := range jobs {
		select {
		case <-runCtx.Done():
			break producer
		case jobsCh <- j:
		}
	}
	close(jobsCh)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return ErrPoolCanceled
	}
	return nil
}
