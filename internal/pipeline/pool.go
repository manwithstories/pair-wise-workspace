// Package pool runs sample validation on a bounded worker pool. Three
// independent bounds are enforced: worker count, queued job count and total
// buffered payload bytes. Context cancellation propagates to producers
// (blocked acquires/sends), workers and the downstream consumer.
package pipeline

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
)

// Config is the validated pool configuration.
type Config struct {
	Workers          int
	Queue            int
	MaxBufferedBytes int64
}

// Validate enforces positive bounds.
func (c Config) Validate() error {
	if c.Workers <= 0 {
		return model.NewError(model.StagePool, "workers must be > 0")
	}
	if c.Queue <= 0 {
		return model.NewError(model.StagePool, "queue length must be > 0")
	}
	if c.MaxBufferedBytes <= 0 {
		return model.NewError(model.StagePool, "buffer memory must be > 0")
	}
	return nil
}

// Job is one unit of work. Order is the global, single-threaded submission
// index; merge uses it to make results independent of worker scheduling.
type Job struct {
	Order  uint64
	Sample clock.NormalizedSample
}

// Result is the validated job. Validation failures are carried in Err rather
// than closing the pipeline; the writer decides recover vs abort.
type Result struct {
	Order  uint64
	Sample clock.NormalizedSample
	Err    *model.Error
}

// semaphore is a context-aware weighted byte semaphore.
type semaphore struct {
	tokens chan struct{}
}

func newSemaphore(weight int64) *semaphore {
	if weight <= 0 {
		weight = 1
	}
	return &semaphore{tokens: make(chan struct{}, weight)}
}

func (s *semaphore) acquire(ctx context.Context, weight int) error {
	for i := 0; i < weight; i++ {
		select {
		case s.tokens <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *semaphore) release(weight int) {
	for i := 0; i < weight; i++ {
		select {
		case <-s.tokens:
		default:
			return // never block shutdown
		}
	}
}

// Pool owns the worker goroutines and the memory budget.
type Pool struct {
	cfg Config
	mem *semaphore
}

// New validates the config and constructs the pool.
func New(cfg Config) (*Pool, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Pool{cfg: cfg, mem: newSemaphore(cfg.MaxBufferedBytes)}, nil
}

// Acquire reserves payload bytes before a producer enqueues a job. It blocks
// while the memory budget is exhausted and returns early on cancellation, so
// cancellation reaches the producer side.
func (p *Pool) Acquire(ctx context.Context, weight int) error {
	if int64(weight) > p.cfg.MaxBufferedBytes {
		return model.NewError(model.StagePool,
			fmt.Sprintf("single sample %d bytes exceeds buffer limit %d", weight, p.cfg.MaxBufferedBytes))
	}
	return p.mem.acquire(ctx, weight)
}

// Release returns a job's payload budget after it has been consumed.
func (p *Pool) Release(weight int) { p.mem.release(weight) }

// Run spawns Workers goroutines reading jobs and returns a results channel
// closed after every worker exits. The queue bound is the channel capacity;
// backpressure on it, on the memory semaphore and on ctx all gate the
// producer. Workers stop promptly when ctx is cancelled and always release
// the memory budget of any in-flight job.
func (p *Pool) Run(ctx context.Context, jobs <-chan Job) <-chan Result {
	results := make(chan Result, p.cfg.Queue)
	var wg sync.WaitGroup
	wg.Add(p.cfg.Workers)
	for w := 0; w < p.cfg.Workers; w++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					res := Result{Order: job.Order, Sample: job.Sample}
					if err := Validate(job.Sample.Sample); err != nil {
						res.Err = err
					} else {
						res.Sample.SHA256 = sha256.Sum256(job.Sample.Payload)
					}
					select {
					case results <- res:
						p.Release(len(job.Sample.Payload))
					case <-ctx.Done():
						p.Release(len(job.Sample.Payload))
						return
					}
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	return results
}

// Validate checks one sample's structural integrity. Truncated samples are
// flagged as recoverable damage, not hard failures.
func Validate(s demux.Sample) *model.Error {
	if s.Size == 0 && s.Flags&demux.FlagTruncated == 0 {
		return model.NewError(model.StagePool,
			fmt.Sprintf("zero-size sample: track=%d seq=%d", s.TrackID, s.Seq)).
			WithTrack(int32(s.TrackID)).WithOffset(s.Offset).WithSample(s.Seq)
	}
	return nil
}
