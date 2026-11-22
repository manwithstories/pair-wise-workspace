package pipe

import (
	"sync"
	"time"

	"mediasim/internal/txn"
)

// RunStats summarises one pipeline run.
type RunStats struct {
	Latencies  []time.Duration // enqueue-to-commit per committed segment
	Aborted    int             // segments aborted on backpressure timeout
	PeakBytes  int             // peak queue occupancy
	SubmitErrs int             // segments rejected by validation
}

// Runner pushes segments through a bounded queue to a delayed consumer
// that commits them. Backpressure from a slow consumer propagates to the
// producer; on push timeout the current transaction is aborted and the
// last valid commit point is preserved.
type Runner struct {
	queue         *Queue
	consumerDelay time.Duration
	pushTimeout   time.Duration
}

// NewRunner builds a runner over a queue of maxBytes, a per-item consumer
// delay, and a producer push timeout.
func NewRunner(maxBytes int, consumerDelay, pushTimeout time.Duration) *Runner {
	return &Runner{
		queue:         NewQueue(maxBytes),
		consumerDelay: consumerDelay,
		pushTimeout:   pushTimeout,
	}
}

// Run feeds all segments through the queue and returns after the queue
// is drained.
func (r *Runner) Run(segs []txn.Segment, c *txn.Committer) RunStats {
	var stats RunStats
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			it, ok := r.queue.Pop()
			if !ok {
				return
			}
			if r.consumerDelay > 0 {
				time.Sleep(r.consumerDelay)
			}
			err := c.Submit(it.Segment)
			mu.Lock()
			if err != nil {
				stats.SubmitErrs++
			} else {
				stats.Latencies = append(stats.Latencies, time.Since(it.EnqueuedAt))
			}
			mu.Unlock()
		}
	}()
	for _, seg := range segs {
		it := Item{
			Segment:    seg,
			Size:       len(seg.Payload) + 64,
			EnqueuedAt: time.Now(),
		}
		if err := r.queue.Push(it, r.pushTimeout); err != nil {
			c.Abort(seg.ID)
			mu.Lock()
			stats.Aborted++
			mu.Unlock()
		}
	}
	r.queue.Close()
	<-done
	stats.PeakBytes = r.queue.Peak()
	return stats
}
