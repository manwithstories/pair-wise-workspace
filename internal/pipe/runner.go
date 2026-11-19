package pipe

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

// Stats summarizes one pipeline run.
type Stats struct {
	Produced  int
	Consumed  int
	Latencies []time.Duration // enqueue-to-ack per consumed item
}

// P95Latency returns the 95th-percentile enqueue-to-ack latency.
func (s *Stats) P95Latency() time.Duration {
	if len(s.Latencies) == 0 {
		return 0
	}
	ls := append([]time.Duration(nil), s.Latencies...)
	sort.Slice(ls, func(i, j int) bool { return ls[i] < ls[j] })
	idx := (len(ls)*95 + 99) / 100 // ceil(0.95*n), 1-based
	return ls[idx-1]
}

// Runner feeds packets through a bounded queue to a consumer.
type Runner struct {
	Capacity      int
	ConsumerDelay time.Duration // artificial per-item consumer cost
	SlowTimeout   time.Duration // max time between acks before cleanup
}

// ConsumeFunc processes one item; a non-nil error aborts the pipeline.
type ConsumeFunc func(ctx context.Context, it Item) error

// Run pushes every packet through the queue and consumes them. It always
// releases queue resources; on abnormal termination the last acknowledged
// packet remains available via the returned queue.
func (r *Runner) Run(ctx context.Context, packets []scan.Packet, consume ConsumeFunc) (*Stats, *Queue, fault.List) {
	q := NewQueue(r.Capacity)
	stats := &Stats{}
	var faults fault.List

	slow := r.SlowTimeout
	if slow <= 0 {
		slow = 30 * time.Second
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // producer with backpressure
		defer wg.Done()
		defer q.Close()
		for _, p := range packets {
			if err := q.Push(ctx, Item{Packet: p}); err != nil {
				return // abort or cancel already recorded by consumer side
			}
			stats.Produced++
		}
	}()

	ack := make(chan time.Duration, 1)
	lastProgress := time.Now()
	var progressMu sync.Mutex

	wg.Add(1)
	go func() { // consumer
		defer wg.Done()
		for {
			it, ok := q.Pop(ctx)
			if !ok {
				return
			}
			if r.ConsumerDelay > 0 {
				t := time.NewTimer(r.ConsumerDelay)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
					return
				}
			}
			if err := consume(ctx, it); err != nil {
				faults = append(faults, fault.New(fault.KindConsumerError, fault.StageQueue,
					it.Packet.TrackID, it.Packet.PTS, it.Packet.Offset, false, err.Error()))
				q.Abort(err)
				return
			}
			q.Ack(it)
			stats.Consumed++
			select {
			case ack <- time.Since(it.EnqueuedAt):
			default:
			}
			progressMu.Lock()
			lastProgress = time.Now()
			progressMu.Unlock()
		}
	}()

	// Watchdog: clean up a slow consumer.
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(slow / 4)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case d := <-ack:
				stats.Latencies = append(stats.Latencies, d)
			case <-t.C:
				progressMu.Lock()
				stalled := time.Since(lastProgress) > slow
				progressMu.Unlock()
				if stalled {
					faults = append(faults, fault.New(fault.KindConsumerSlow, fault.StageQueue,
						0, 0, -1, false, "consumer made no progress within timeout"))
					q.Abort(errors.New("slow consumer"))
					cancel()
					return
				}
			}
		}
	}()

	wg.Wait()
	cancel()
	<-done

	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		faults = append(faults, fault.New(fault.KindCanceled, fault.StageQueue, 0, 0, -1, false, err.Error()))
	}
	if q.AbortErr() == nil && ctx.Err() != nil && stats.Consumed < stats.Produced {
		faults = append(faults, fault.New(fault.KindCanceled, fault.StageQueue, 0, 0, -1, false, "pipeline canceled"))
	}
	return stats, q, faults
}
