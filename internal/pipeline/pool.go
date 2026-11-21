// Package pipeline provides a bounded-concurrency worker pool and a
// deterministic merger. Memory is bounded by the queue capacity times
// the maximum packet-group size; slow consumers push backpressure all
// the way to the reader. Context cancellation wakes every blocked
// goroutine and reclaims the pool.
package pipeline

import (
	"context"
	"sync"
	"sync/atomic"

	"tsbatch/internal/inspect"
	"tsbatch/internal/ts"
)

// Job is one numbered group of packets in input order.
type Job struct {
	Seq         uint64
	FirstPacket uint64
	Packets     [][ts.PacketSize]byte
}

// Result carries the parsed events of one group.
type Result struct {
	Seq    uint64
	Events []inspect.PacketEvent
	Err    error
}

// Pool parses packet groups with a fixed number of workers.
type Pool struct {
	ctx     context.Context
	jobs    chan Job
	results chan Result
	wg      sync.WaitGroup
	peak    atomic.Int64
}

// NewPool starts workers goroutines bound to ctx. queue is the maximum
// number of unprocessed jobs held between reader and workers.
func NewPool(ctx context.Context, workers, queue int) *Pool {
	p := &Pool{
		ctx:     ctx,
		jobs:    make(chan Job, queue),
		results: make(chan Result, queue),
	}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case j, ok := <-p.jobs:
			if !ok {
				return
			}
			p.observe(len(p.jobs))
			select {
			case p.results <- p.process(j):
			case <-p.ctx.Done():
				return
			}
		}
	}
}

func (p *Pool) process(j Job) (r Result) {
	r.Seq = j.Seq
	for i := range j.Packets {
		ev, err := inspect.ParsePacket(j.FirstPacket+uint64(i), &j.Packets[i])
		if err != nil {
			r.Err = err
			return r
		}
		r.Events = append(r.Events, ev)
	}
	return r
}

// Submit enqueues a job, blocking when the queue is full (backpressure)
// and returning ctx.Err() on cancellation.
func (p *Pool) Submit(j Job) error {
	select {
	case p.jobs <- j:
		p.observe(len(p.jobs))
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

// Results returns the unordered worker output.
func (p *Pool) Results() <-chan Result { return p.results }

// Close closes the job queue, waits for workers to finish and closes the
// results channel. Safe to call after cancellation.
func (p *Pool) Close() {
	close(p.jobs)
	p.wg.Wait()
	close(p.results)
}

// Peak returns the maximum observed queue occupancy (never exceeds the
// queue capacity).
func (p *Pool) Peak() int64 { return p.peak.Load() }

func (p *Pool) observe(d int) {
	d64 := int64(d)
	for {
		pk := p.peak.Load()
		if d64 <= pk || p.peak.CompareAndSwap(pk, d64) {
			return
		}
	}
}
