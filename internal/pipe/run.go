package pipe

import (
	"context"
	"sync"

	"medpipe/internal/clock"
)

// StageFunc processes one item and returns zero or more downstream items.
// A non-nil error aborts the pipeline; a write interruption must be returned
// rather than swallowed.
type StageFunc func(ctx context.Context, in *Item) ([]*Item, error)

// Stage binds a name, worker count and handler.
type Stage struct {
	Name    string
	Workers int
	Run     StageFunc
}

// Metrics summarizes pipeline execution.
type Metrics struct {
	// PeakInflight is the highest sum of in-flight bytes across every
	// stage boundary at once, proving memory does not grow with duration.
	PeakInflight int64
	// SinkBytes is the total bytes handed to the final-stage sink.
	SinkBytes int64
	Processed int
	Failures  int
}

// Runner wires stages through one input queue per stage. The per-queue
// capacities partition a single global in-flight byte budget.
type Runner struct {
	queues    []*Queue // len == len(stages); stage i reads queues[i]
	stages    []Stage
	budget    int64
	maxItems  int
	mu        sync.Mutex
	processed int
	sinkBytes int64
}

// DefaultMaxConcurrency caps the total worker count across all stages so a
// stage configuration cannot spawn unbounded goroutines on a mobile device.
const DefaultMaxConcurrency = 16

// MaxConcurrency is the enforced total-worker ceiling. Tests may lower it;
// production code should leave it at DefaultMaxConcurrency.
var MaxConcurrency = DefaultMaxConcurrency

// NewRunner builds a runner with one global in-flight byte budget shared
// across all stage input queues. The summed worker count must not exceed
// MaxConcurrency, otherwise the configuration is rejected.
func NewRunner(budget int64, maxItems int, stages []Stage) (*Runner, error) {
	if len(stages) == 0 {
		return nil, clock.E(clock.KindArgument, "pipe.runner",
			clock.WithDetail("at least one stage required"))
	}
	if maxItems <= 0 {
		return nil, clock.E(clock.KindArgument, "pipe.runner",
			clock.WithDetail("maxItems must be positive"))
	}
	totalWorkers := 0
	for _, s := range stages {
		if s.Workers <= 0 {
			return nil, clock.E(clock.KindArgument, "pipe.runner",
				clock.WithDetail("stage %s needs >=1 worker", s.Name))
		}
		totalWorkers += s.Workers
	}
	if totalWorkers > MaxConcurrency {
		return nil, clock.E(clock.KindArgument, "pipe.runner",
			clock.WithDetail("total workers %d exceed max concurrency %d",
				totalWorkers, MaxConcurrency))
	}
	perQueue := budget / int64(len(stages))
	if perQueue <= 0 {
		perQueue = budget
	}
	queues := make([]*Queue, len(stages))
	for i := range queues {
		q, err := NewQueue(perQueue)
		if err != nil {
			return nil, err
		}
		queues[i] = q
	}
	return &Runner{queues: queues, stages: stages, budget: budget, maxItems: maxItems}, nil
}

// Run feeds seed items into the first stage, executes every stage to
// completion and returns the first error. Each stage's input queue is closed
// once its upstream is fully done, so workers drain and exit without hanging.
// On error or cancellation every queue closes immediately.
func (r *Runner) Run(ctx context.Context, seeds []*Item) (Metrics, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() {
			firstErr = err
			cancel()
			for _, q := range r.queues {
				q.Close()
			}
		})
	}

	var stageDone sync.WaitGroup
	for i, stage := range r.stages {
		stageDone.Add(1)
		r.spawnStage(ctx, i, stage, &stageDone, fail)
	}

	// Feed seeds under backpressure, then close stage 0's queue so its
	// workers drain and exit, which cascades Close through every stage.
	go func() {
		for _, it := range seeds {
			if err := r.queues[0].Put(ctx, it); err != nil {
				fail(err)
				return
			}
		}
		r.queues[0].Close()
	}()

	stageDone.Wait()
	for _, q := range r.queues {
		q.Close()
	}

	r.mu.Lock()
	metrics := Metrics{
		PeakInflight: r.peakInflight(),
		SinkBytes:    r.sinkBytes,
		Processed:    r.processed,
		Failures:     boolToInt(firstErr != nil),
	}
	r.mu.Unlock()
	return metrics, firstErr
}

func (r *Runner) spawnStage(ctx context.Context, idx int, s Stage,
	stageDone *sync.WaitGroup, fail func(error)) {
	in := r.queues[idx]
	last := idx == len(r.stages)-1

	var workerDone sync.WaitGroup
	for w := 0; w < s.Workers; w++ {
		workerDone.Add(1)
		go func() {
			defer workerDone.Done()
			for {
				item, ok, err := in.Get(ctx)
				if err != nil {
					fail(err)
					return
				}
				if !ok {
					return
				}
				produced, runErr := s.Run(ctx, item)
				if runErr != nil {
					fail(runErr)
					return
				}
				if last {
					r.accountSink(produced)
					r.mu.Lock()
					r.processed++
					r.mu.Unlock()
					continue
				}
				for _, next := range produced {
					next.Stage = s.Name
					if err := r.queues[idx+1].Put(ctx, next); err != nil {
						fail(err)
						return
					}
				}
			}
		}()
	}
	// Coordinator: close this stage's input once all its workers finish,
	// which happens only after the upstream closed the queue and it drained.
	go func() {
		defer stageDone.Done()
		workerDone.Wait()
		if !last {
			r.queues[idx+1].Close()
		}
	}()
}

func (r *Runner) accountSink(items []*Item) {
	r.mu.Lock()
	for _, it := range items {
		r.sinkBytes += it.Bytes
	}
	r.mu.Unlock()
}

func (r *Runner) peakInflight() int64 {
	var sum int64
	for _, q := range r.queues {
		sum += q.PeakInflight()
	}
	return sum
}

// MaxItems returns the configured item cap.
func (r *Runner) MaxItems() int { return r.maxItems }

// Budget returns the global in-flight byte budget.
func (r *Runner) Budget() int64 { return r.budget }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
