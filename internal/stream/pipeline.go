// Package stream runs the media core as a bounded producer/consumer pipeline:
// the scanner feeds an index, the executor decodes and normalizes audio, and a
// bounded queue carries work to a segment writer.
//
// Backpressure is mandatory. The queue's byte budget (64 MiB by default) is
// reserved before a job is enqueued; a slow consumer therefore blocks the
// producer instead of letting memory grow. Cancellation releases every reserved
// byte and preserves the last acknowledged checkpoint.
package stream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrQueueClosed is returned when a producer sends after close.
var ErrQueueClosed = errors.New("stream: queue closed")

// ErrCancelled is returned when the caller's context ends.
var ErrCancelled = context.Canceled

// ErrMemoryLimit reports a single job larger than the queue budget.
var ErrMemoryLimit = errors.New("stream: job exceeds memory limit")

// MemoryLimit is the default queue budget: 64 MiB.
const MemoryLimit int64 = 64 << 20

// Job is one unit of work moving through the bounded queue. Cost is the
// reserved budget in bytes; it must cover every buffer the job keeps alive.
type Job struct {
	ID      int
	StartUS int64
	EndUS   int64
	Cost    int64
	// Acknowledged marks a job whose output has been flushed and whose
	// checkpoint may safely advance.
	Acknowledged bool
}

// Queue is a byte-bounded FIFO with explicit, fail-fast close and a peak usage
// counter used by the memory assertion.
type Queue struct {
	mu       sync.Mutex
	notFull  *sync.Cond
	notEmpty *sync.Cond
	jobs     []*Job
	used     int64
	limit    int64
	peak     int64
	closed   bool
	// blocked counts producers currently waiting on backpressure, so tests
	// can observe that a slow consumer really pushed back.
	blocked atomic.Int64
	// waits counts backpressure waits; deliberately observable.
	waits atomic.Int64
}

// NewQueue creates a queue with a byte budget.
func NewQueue(limit int64) *Queue {
	if limit <= 0 {
		limit = MemoryLimit
	}
	q := &Queue{limit: limit}
	q.notFull = sync.NewCond(&q.mu)
	q.notEmpty = sync.NewCond(&q.mu)
	return q
}

// Limit returns the queue budget.
func (q *Queue) Limit() int64 { return q.limit }

// Used returns the currently reserved bytes.
func (q *Queue) Used() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.used
}

// Peak returns the maximum reserved bytes ever observed.
func (q *Queue) Peak() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peak
}

// Blocked returns how many producers are waiting for room right now.
func (q *Queue) Blocked() int64 { return q.blocked.Load() }

// Waits returns the total number of backpressure waits.
func (q *Queue) Waits() int64 { return q.waits.Load() }

// Push reserves cost and enqueues the job. It blocks while there is not enough
// room and returns ctx.Err() if the context ends first. A push larger than the
// whole budget is rejected immediately instead of blocking forever.
func (q *Queue) Push(ctx context.Context, job *Job) error {
	if job == nil {
		return errors.New("stream: nil job")
	}
	if job.Cost < 0 {
		return fmt.Errorf("%w: negative cost %d", ErrMemoryLimit, job.Cost)
	}
	if job.Cost > q.limit {
		return fmt.Errorf("%w: job %d costs %d > %d", ErrMemoryLimit, job.ID, job.Cost, q.limit)
	}
	// Watch the context so a blocked producer wakes on cancel.
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.notFull.Broadcast()
		q.notEmpty.Broadcast()
		q.mu.Unlock()
	})
	defer stop()

	q.mu.Lock()
	waited := false
	leave := func() {
		if waited {
			q.blocked.Add(-1)
		}
		q.mu.Unlock()
	}
	for {
		if q.closed {
			leave()
			return ErrQueueClosed
		}
		if err := ctx.Err(); err != nil {
			leave()
			return err
		}
		if q.used+job.Cost <= q.limit || len(q.jobs) == 0 {
			// An empty queue always accepts, so a job exactly at the limit
			// cannot deadlock against its own reservation.
			break
		}
		if !waited {
			q.blocked.Add(1)
			waited = true
		}
		q.waits.Add(1)
		q.notFull.Wait()
	}
	if waited {
		q.blocked.Add(-1)
	}
	q.used += job.Cost
	if q.used > q.peak {
		q.peak = q.used
	}
	q.jobs = append(q.jobs, job)
	q.notEmpty.Signal()
	q.mu.Unlock()
	return nil
}

// Pop removes the next job, blocking until one is available, the queue is
// closed and drained, or the context ends.
func (q *Queue) Pop(ctx context.Context) (*Job, error) {
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.notEmpty.Broadcast()
		q.notFull.Broadcast()
		q.mu.Unlock()
	})
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(q.jobs) > 0 {
			job := q.jobs[0]
			q.jobs[0] = nil
			q.jobs = q.jobs[1:]
			q.used -= job.Cost
			if q.used < 0 {
				q.used = 0
			}
			q.notFull.Broadcast()
			return job, nil
		}
		if q.closed {
			return nil, ErrQueueClosed
		}
		q.notEmpty.Wait()
	}
}

// Ack acknowledges a popped job. Acknowledged jobs are the only ones a
// checkpoint may cover, so a cancelled run never reports unflushed work.
func (q *Queue) Ack(job *Job) {
	if job == nil {
		return
	}
	q.mu.Lock()
	job.Acknowledged = true
	q.mu.Unlock()
}

// Close stops further pushes. Popped jobs are still drained by consumers.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.notEmpty.Broadcast()
	q.notFull.Broadcast()
	q.mu.Unlock()
}

// Reset releases all reservations; used on cancellation so memory returns to
// zero immediately rather than waiting for garbage collection.
func (q *Queue) Reset() {
	q.mu.Lock()
	q.jobs = nil
	q.used = 0
	q.closed = true
	q.notEmpty.Broadcast()
	q.notFull.Broadcast()
	q.mu.Unlock()
}

// RunOptions configures the producer/consumer loop.
type RunOptions struct {
	// Concurrency is the number of consumer goroutines; defaults to 1 so
	// segment output stays ordered and deterministic.
	Concurrency int
	// ConsumerDelay simulates a slow consumer (tests and the demo flag).
	ConsumerDelay time.Duration
	// StopAfter cancels the run after this many delivered jobs, simulating a
	// crash; zero means run to completion.
	StopAfter int
}

// Outcome summarises a run.
type Outcome struct {
	Processed         int
	Acknowledged      int
	LastAckUS         int64
	PeakBytes         int64
	BackpressureWaits int64
	Cancelled         bool
}

// Run drives producers through a bounded queue to consumers. produce is called
// with a send function; consume receives each job in order.
//
// Checkpoints advance only after consume returns nil and the job is
// acknowledged, so a cancelled run leaves LastAckUS at the last fully written
// segment.
func Run(ctx context.Context, q *Queue, jobs []Job, consume func(context.Context, *Job) error, opt RunOptions) (Outcome, error) {
	if opt.Concurrency <= 0 {
		opt.Concurrency = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var out Outcome
	var ackMu sync.Mutex
	errCh := make(chan error, opt.Concurrency+1)
	var wg sync.WaitGroup
	delivered := atomic.Int64{}

	for i := 0; i < opt.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, err := q.Pop(ctx)
				if err != nil {
					if errors.Is(err, ErrQueueClosed) || ctx.Err() != nil {
						return
					}
					errCh <- err
					return
				}
				if opt.ConsumerDelay > 0 {
					select {
					case <-time.After(opt.ConsumerDelay):
					case <-ctx.Done():
						return
					}
				}
				if err := consume(ctx, job); err != nil {
					errCh <- err
					cancel()
					return
				}
				q.Ack(job)
				ackMu.Lock()
				out.Processed++
				out.Acknowledged++
				if job.EndUS > out.LastAckUS {
					out.LastAckUS = job.EndUS
				}
				ackMu.Unlock()
				n := delivered.Add(1)
				if opt.StopAfter > 0 && int(n) >= opt.StopAfter {
					cancel()
					return
				}
			}
		}()
	}

	producerErr := make(chan error, 1)
	go func() {
		defer q.Close()
		for i := range jobs {
			job := &jobs[i]
			if err := q.Push(ctx, job); err != nil {
				producerErr <- err
				return
			}
		}
		producerErr <- nil
	}()

	perr := <-producerErr
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil && !errors.Is(err, context.Canceled) {
			return out, err
		}
	}
	// Yield every reserved byte on the way out: a cancelled run must not keep
	// memory pinned in undelivered jobs.
	q.Reset()
	out.PeakBytes = q.Peak()
	out.BackpressureWaits = q.Waits()
	out.Cancelled = ctx.Err() != nil
	if errors.Is(perr, context.Canceled) {
		return out, ErrCancelled
	}
	if perr != nil {
		return out, perr
	}
	if out.Cancelled && opt.StopAfter > 0 {
		return out, ErrCancelled
	}
	return out, nil
}

// SegmentBounds splits a duration into fixed-size segments. The final segment
// is clamped to the duration so segments never extend past available media.
func SegmentBounds(durationUS int64, sizeUS int64) ([]Job, error) {
	if durationUS < 0 {
		return nil, fmt.Errorf("%w: duration %d", ErrMemoryLimit, durationUS)
	}
	if sizeUS <= 0 {
		return nil, fmt.Errorf("%w: segment size %d", ErrMemoryLimit, sizeUS)
	}
	var jobs []Job
	for start := int64(0); start < durationUS; start += sizeUS {
		end := start + sizeUS
		if end > durationUS {
			end = durationUS
		}
		jobs = append(jobs, Job{ID: len(jobs), StartUS: start, EndUS: end})
	}
	return jobs, nil
}
