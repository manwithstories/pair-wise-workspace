// Package flow moves segment jobs from per-representation producers to
// per-bitrate workers through byte-bounded queues. A slow consumer
// triggers backpressure on the producers; cancellation and write
// failures propagate along the pipeline and release every queue.
package flow

import (
	"context"
	"errors"
	"sync"

	"mediaroute/internal/model"
)

// ErrClosed is returned by Put/Get on a queue released by Close.
var ErrClosed = errors.New("flow: queue closed")

// Job is one segment of one representation flowing through a queue.
type Job struct {
	Camera      string
	Bitrate     uint32
	Segment     int
	StartClock  int64 // shared 90 kHz clock of the first sample
	EndClock    int64 // shared clock just past the last sample
	Source      string
	Timescale   uint32
	StartSample uint32         // index of the first sample in the source track
	Samples     []model.Sample // non-truncated samples of this segment
	Payload     []byte         // concatenated sample payloads
}

// Bytes is the queue-accounted size of the job (resident payload bytes).
func (j Job) Bytes() int64 { return int64(len(j.Payload)) }

// Queue is a byte-bounded blocking job queue. The hard cap is on the
// sum of Bytes() of queued jobs; producers block (backpressure) while
// the cap would be exceeded.
type Queue struct {
	name string
	cap  int64

	mu           sync.Mutex
	cond         *sync.Cond
	items        []Job
	used         int64
	peak         int64
	backpressure uint64
	closed       bool
	err          error
}

// NewQueue creates a queue with a hard byte cap.
func NewQueue(name string, capBytes int64) *Queue {
	q := &Queue{name: name, cap: capBytes}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Put enqueues j, blocking while the byte cap would be exceeded. It
// returns early with the queue error when the queue is released.
func (q *Queue) Put(j Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	waited := false
	for !q.closed && q.used+j.Bytes() > q.cap {
		if !waited { // count one backpressure event per blocked Put
			q.backpressure++
			waited = true
		}
		q.cond.Wait()
	}
	if q.closed {
		return q.closeErr()
	}
	q.items = append(q.items, j)
	q.used += j.Bytes()
	if q.used > q.peak {
		q.peak = q.used
	}
	q.cond.Signal()
	return nil
}

// Get dequeues the oldest job, blocking while the queue is empty.
func (q *Queue) Get() (Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for !q.closed && len(q.items) == 0 {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return Job{}, q.closeErr()
	}
	j := q.items[0]
	q.items[0] = Job{} // release payload reference
	q.items = q.items[1:]
	q.used -= j.Bytes()
	q.cond.Broadcast() // wake blocked producers
	return j, nil
}

// Close releases the queue: blocked Puts and Gets return, further Puts
// fail, and remaining items stay drainable via Get. err is propagated
// to all waiters (nil becomes ErrClosed).
func (q *Queue) Close(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if err == nil {
		err = ErrClosed
	}
	q.closed = true
	q.err = err
	q.cond.Broadcast()
}

func (q *Queue) closeErr() error {
	if q.err != nil {
		return q.err
	}
	return ErrClosed
}

// Stats reports the peak queued bytes and backpressure event count.
func (q *Queue) Stats() (peakBytes int64, backpressure uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peak, q.backpressure
}

// WatchCancel closes the queue with ctx.Err() when ctx is cancelled,
// propagating cancellation along the pipeline. Returns a stop func.
func (q *Queue) WatchCancel(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.Close(ctx.Err())
		case <-done:
		}
	}()
	return func() { close(done) }
}
