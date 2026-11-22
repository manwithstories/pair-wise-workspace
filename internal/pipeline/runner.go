package pipeline

import (
	"context"
	"sync"
)

// Checkpoint is the last fully committed, verified boundary. On cancellation
// everything after it is discarded; a restart resumes from TimeNS.
type Checkpoint struct {
	SegmentSeq int
	TimeNS     int64
	Fingerprint string
}

// Runner wires one bounded queue between a producer goroutine and a consumer
// function. It tracks the last committed checkpoint and guarantees that
// cancellation leaves only complete segments before the boundary.
type Runner struct {
	queue *Queue

	mu         sync.Mutex
	checkpoint Checkpoint
	committed  int
	inflight   int
	canceled   bool
}

// NewRunner binds a runner to a bounded queue.
func NewRunner(q *Queue) *Runner {
	return &Runner{queue: q, checkpoint: Checkpoint{SegmentSeq: -1}}
}

// Queue exposes the underlying queue for the producer.
func (r *Runner) Queue() *Queue { return r.queue }

// Run executes consumer for every frame until ctx ends, the producer calls
// Close, or consumer returns an error. FrameSegmentCommit frames advance the
// checkpoint atomically; partial work after the last commit is abandoned.
func (r *Runner) Run(ctx context.Context, consumer func(Frame) error) error {
	for {
		f, err := r.queue.Receive(ctx)
		if err != nil {
			r.mu.Lock()
			r.canceled = ctx.Err() != nil
			r.mu.Unlock()
			if err == ErrQueueClosed {
				return nil
			}
			return err
		}
		if f.Kind == FrameSegmentCommit {
			if err := consumer(f); err != nil {
				return err
			}
			r.mu.Lock()
			r.committed++
			r.checkpoint = Checkpoint{
				SegmentSeq: r.committed - 1,
				TimeNS:     f.TimeNS,
				Fingerprint: f.Meta["fingerprint"],
			}
			r.inflight = 0
			r.mu.Unlock()
			continue
		}
		if err := consumer(f); err != nil {
			return err
		}
		r.mu.Lock()
		r.inflight++
		r.mu.Unlock()
	}
}

// Checkpoint returns the latest verified boundary.
func (r *Runner) Checkpoint() Checkpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checkpoint
}

// Committed counts fully committed segments.
func (r *Runner) Committed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.committed
}

// Canceled reports whether the runner stopped due to context cancellation.
func (r *Runner) Canceled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.canceled
}

// ResumeFrom is the boundary a restart must continue after.
func (r *Runner) ResumeFrom() Checkpoint { return r.Checkpoint() }
