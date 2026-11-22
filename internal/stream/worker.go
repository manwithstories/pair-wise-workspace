package stream

import (
	"context"
	"sync"
	"time"
)

// ProcessFunc is one deterministic pipeline stage. It receives an item
// and returns zero or more output items. Errors cancel the stage and are
// surfaced to the Run owner; returning no outputs simply drops the item
// (used by validation stages that quarantine bad input).
type ProcessFunc func(ctx context.Context, in Item) ([]Item, error)

// StageConfig parameterizes a worker.
type StageConfig struct {
	Name string
	// ProcessDelay is added to every item to emulate a slow archive disk
	// / slow consumer. Zero means no artificial delay.
	ProcessDelay time.Duration
}

// Stage joins an input queue to an output queue through one worker
// goroutine. A nil output queue means the stage is a terminal sink.
type Stage struct {
	cfg    StageConfig
	in     *BoundedQueue
	out    *BoundedQueue
	process ProcessFunc

	mu        sync.Mutex
	processed int64
	errors    int64
	latencies []time.Duration // bounded ring of recent latencies
}

// NewStage wires a processing function between two queues.
func NewStage(cfg StageConfig, in, out *BoundedQueue, process ProcessFunc) *Stage {
	return &Stage{
		cfg:       cfg,
		in:        in,
		out:       out,
		process:   process,
		latencies: make([]time.Duration, 0, 1024),
	}
}

// Run blocks until the input queue is drained and closed, the context is
// cancelled, or the processing function returns an error. On normal
// completion the output queue is closed so downstream stages terminate.
func (s *Stage) Run(ctx context.Context) error {
	for {
		start := time.Now()
		item, ok, err := s.in.Dequeue(ctx)
		if err != nil {
			return err
		}
		if !ok {
			// Input closed and fully drained.
			if s.out != nil {
				s.out.Close()
			}
			return nil
		}
		if s.cfg.ProcessDelay > 0 {
			select {
			case <-time.After(s.cfg.ProcessDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		outputs, err := s.process(ctx, item)
		if err != nil {
			s.mu.Lock()
			s.errors++
			s.mu.Unlock()
			return err
		}
		for _, o := range outputs {
			if s.out == nil {
				break
			}
			if err := s.out.Enqueue(ctx, o); err != nil {
				return err
			}
		}
		lat := time.Since(start)
		s.mu.Lock()
		s.processed++
		if len(s.latencies) < cap(s.latencies) {
			s.latencies = append(s.latencies, lat)
		} else {
			s.latencies[s.processed%int64(cap(s.latencies))] = lat
		}
		s.mu.Unlock()
	}
}

// Stats is a read-only stage counter snapshot.
type Stats struct {
	Name      string
	Processed int64
	Errors    int64
}

// Snapshot returns the stage counters.
func (s *Stage) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Name: s.cfg.Name, Processed: s.processed, Errors: s.errors}
}

// LatencyP95 returns the 95th percentile of observed per-item latencies
// over the bounded ring. Zero samples report 0.
func (s *Stage) LatencyP95() time.Duration {
	s.mu.Lock()
	samples := append([]time.Duration(nil), s.latencies...)
	s.mu.Unlock()
	if len(samples) == 0 {
		return 0
	}
	// Insertion-order independent: sort a copy for a deterministic rank.
	for i := 1; i < len(samples); i++ {
		for j := i; j > 0 && samples[j] < samples[j-1]; j-- {
			samples[j], samples[j-1] = samples[j-1], samples[j]
		}
	}
	idx := (len(samples) * 95) / 100
	if idx >= len(samples) {
		idx = len(samples) - 1
	}
	return samples[idx]
}

// Pipeline runs several stages concurrently with the given queues and
// waits for all of them. The first stage error cancels the others via the
// derived context; all queues are closed/drained on the way out so no
// goroutine is left blocked on a full queue.
func Pipeline(ctx context.Context, stages []*Stage, root *BoundedQueue, sinks ...*BoundedQueue) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(stages))
	for _, st := range stages {
		wg.Add(1)
		go func(st *Stage) {
			defer wg.Done()
			if err := st.Run(ctx); err != nil && ctx.Err() == nil {
				errCh <- err
				cancel()
			}
		}(st)
	}
	wg.Wait()
	// Defensive cleanup: release anything still buffered on cancellation.
	root.DrainAndRelease()
	for _, q := range sinks {
		q.DrainAndRelease()
	}
	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
	}
}
