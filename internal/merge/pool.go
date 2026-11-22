// Package merge reassembles parsed RTP/RTCP records into a single
// deterministic, clock-calibrated timeline. Decode work runs on a
// bounded worker pool; reordering, deduplication and cross-track merge
// are deterministic functions of the input, so identical input and
// parameters always yield identical output.
package merge

import (
	"context"
	"sync"

	"medpipe/internal/fault"
	rtpstream "medpipe/internal/rtp"
)

// Hard limits for the worker pool.
const (
	MaxWorkers = 64
	MaxQueue   = 8192
)

// Job is one unit of decode work. Index is the record's position in
// the input stream; callers use it to restore stream order after
// concurrent decoding.
type Job struct {
	Index uint64
	Rec   *rtpstream.Record
}

// Pool decodes records concurrently with bounded concurrency and a
// bounded job queue. A slow consumer backpressures Submit; cancelling
// the context makes every goroutine exit.
type Pool struct {
	jobs    chan Job
	results chan Result
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewPool validates the requested limits and starts the workers.
// decode is called once per job from a worker goroutine.
func NewPool(ctx context.Context, workers, queue int, decode func(context.Context, Job) Result) (*Pool, error) {
	if workers < 1 || workers > MaxWorkers {
		return nil, fault.New(fault.KindResource, fault.Unknown, fault.Unknown,
			fault.Unknown, false, "worker count out of range")
	}
	if queue < 1 || queue > MaxQueue {
		return nil, fault.New(fault.KindResource, fault.Unknown, fault.Unknown,
			fault.Unknown, false, "queue capacity out of range")
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &Pool{
		jobs:    make(chan Job, queue),
		results: make(chan Result, queue),
		cancel:  cancel,
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j, ok := <-p.jobs:
					if !ok {
						return
					}
					res := decode(ctx, j)
					select {
					case p.results <- res:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	return p, nil
}

// Submit enqueues a job, blocking while the queue is full. It returns
// the context's error if the context is cancelled first.
func (p *Pool) Submit(ctx context.Context, j Job) error {
	select {
	case p.jobs <- j:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Results returns the channel of decoded results. It is closed by Close
// after all workers have exited.
func (p *Pool) Results() <-chan Result { return p.results }

// Close stops accepting jobs, waits for workers to finish and closes
// the results channel. The consumer must keep draining Results (or the
// context must be cancelled) for Close to return.
func (p *Pool) Close() {
	close(p.jobs)
	p.wg.Wait()
	close(p.results)
	p.cancel()
}
