package stream

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makeJobs(n int, cost int64) []Job {
	jobs := make([]Job, n)
	for i := range jobs {
		jobs[i] = Job{ID: i, StartUS: int64(i) * 1000, EndUS: int64(i+1) * 1000, Cost: cost}
	}
	return jobs
}

func TestQueueRespectsByteLimit(t *testing.T) {
	q := NewQueue(1000)
	ctx := context.Background()
	for _, j := range makeJobs(2, 400) {
		job := j
		if err := q.Push(ctx, &job); err != nil {
			t.Fatal(err)
		}
	}
	if q.Used() != 800 {
		t.Fatalf("usage %d, want 800", q.Used())
	}
	// The next 400 bytes do not fit; with a context timeout the push must
	// block and then report cancellation rather than exceed the budget.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	third := Job{ID: 9, Cost: 400}
	if err := q.Push(short, &third); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("oversubscription: %v", err)
	}
	if q.Used() > q.Limit() {
		t.Fatalf("usage %d exceeded limit", q.Used())
	}
}

func TestQueueBackpressureBlocksProducer(t *testing.T) {
	q := NewQueue(100)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first := Job{ID: 0, Cost: 100}
	if err := q.Push(ctx, &first); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		second := Job{ID: 1, Cost: 100}
		done <- q.Push(ctx, &second)
	}()
	// The producer must be blocked until we free room.
	deadline := time.Now().Add(time.Second)
	for q.Blocked() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if q.Blocked() == 0 {
		t.Fatal("producer was not blocked by backpressure")
	}
	if _, err := q.Pop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("producer did not resume: %v", err)
	}
	if q.Waits() == 0 {
		t.Fatal("backpressure wait not counted")
	}
}

func TestQueueRejectsSingleJobOverLimit(t *testing.T) {
	q := NewQueue(50)
	job := Job{ID: 0, Cost: 51}
	if err := q.Push(context.Background(), &job); !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("oversize job: %v", err)
	}
}

func TestQueueCancelReleasesProducer(t *testing.T) {
	q := NewQueue(100)
	blocked := Job{ID: 0, Cost: 100}
	if err := q.Push(context.Background(), &blocked); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		j := Job{ID: 1, Cost: 100}
		errCh <- q.Push(ctx, &j)
	}()
	deadline := time.Now().Add(time.Second)
	for q.Blocked() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked producer not released by cancellation")
	}
	if q.Blocked() != 0 {
		t.Fatalf("blocked counter leaked: %d", q.Blocked())
	}
}

func TestRunOrdersJobsAndTracksPeak(t *testing.T) {
	q := NewQueue(500)
	jobs := makeJobs(10, 100)
	var seen []int
	var mu sync.Mutex
	out, err := Run(context.Background(), q, jobs, func(_ context.Context, j *Job) error {
		mu.Lock()
		seen = append(seen, j.ID)
		mu.Unlock()
		return nil
	}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range seen {
		if id != i {
			t.Fatalf("out of order: %v", seen)
		}
	}
	if out.PeakBytes > 500 {
		t.Fatalf("peak %d exceeded limit", out.PeakBytes)
	}
	if out.LastAckUS != 10_000 {
		t.Fatalf("last ack %d", out.LastAckUS)
	}
	if q.Used() != 0 {
		t.Fatalf("queue not drained: %d", q.Used())
	}
}

func TestSlowConsumerTriggersBackpressureUnderLimit(t *testing.T) {
	const limit = 1000
	q := NewQueue(limit)
	jobs := makeJobs(20, 100)
	out, err := Run(context.Background(), q, jobs, func(_ context.Context, j *Job) error {
		time.Sleep(time.Millisecond)
		return nil
	}, RunOptions{ConsumerDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if out.PeakBytes > limit {
		t.Fatalf("peak %d > limit %d", out.PeakBytes, limit)
	}
	if out.BackpressureWaits == 0 {
		t.Fatal("slow consumer did not trigger backpressure")
	}
	if out.Acknowledged != len(jobs) {
		t.Fatalf("acknowledged %d/%d", out.Acknowledged, len(jobs))
	}
}

func TestCancellationFreesResourcesAndKeepsAck(t *testing.T) {
	const limit = 400
	q := NewQueue(limit)
	jobs := makeJobs(50, 100)
	out, err := Run(context.Background(), q, jobs, func(_ context.Context, j *Job) error {
		time.Sleep(time.Millisecond)
		return nil
	}, RunOptions{ConsumerDelay: time.Millisecond, StopAfter: 5})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if out.Acknowledged != 5 {
		t.Fatalf("acknowledged %d, want 5", out.Acknowledged)
	}
	if out.LastAckUS != 5000 {
		t.Fatalf("last ack %d, want 5000", out.LastAckUS)
	}
	if got := q.Used(); got != 0 {
		t.Fatalf("queue retained %d bytes after cancellation", got)
	}
	if q.Peak() > limit {
		t.Fatalf("peak %d > %d", q.Peak(), limit)
	}
}

func TestProducerOversizeJobIsRootError(t *testing.T) {
	// A job whose prepared cost exceeds the whole budget must surface as the
	// producer error, not as the empty-queue consequence.
	q := NewQueue(100)
	jobs := makeJobs(2, 10)
	_, err := Run(context.Background(), q, jobs, func(_ context.Context, j *Job) error { return nil },
		RunOptions{Prepare: func(_ context.Context, j *Job) error {
			j.Cost = 1000
			return nil
		}})
	if !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("root cause lost: %v", err)
	}
	if q.Used() != 0 {
		t.Fatalf("queue retained %d bytes", q.Used())
	}
}

func TestConsumerErrorStopsRun(t *testing.T) {
	q := NewQueue(1000)
	boom := errors.New("boom")
	_, err := Run(context.Background(), q, makeJobs(10, 100), func(_ context.Context, j *Job) error {
		if j.ID == 3 {
			return boom
		}
		return nil
	}, RunOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("consumer error lost: %v", err)
	}
}

func TestSegmentBoundsClampTail(t *testing.T) {
	jobs, err := SegmentBounds(2_500_000, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("segments %d", len(jobs))
	}
	if jobs[2].EndUS != 2_500_000 {
		t.Fatalf("tail not clamped: %+v", jobs[2])
	}
	if _, err := SegmentBounds(-1, 1000); err == nil {
		t.Fatal("negative duration accepted")
	}
	if _, err := SegmentBounds(1000, 0); err == nil {
		t.Fatal("zero segment size accepted")
	}
}

func TestQueueResetReleasesAll(t *testing.T) {
	q := NewQueue(1000)
	for i := 0; i < 5; i++ {
		j := Job{ID: i, Cost: 100}
		if err := q.Push(context.Background(), &j); err != nil {
			t.Fatal(err)
		}
	}
	q.Reset()
	if q.Used() != 0 {
		t.Fatalf("reset left %d bytes", q.Used())
	}
	if err := q.Push(context.Background(), &Job{}); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("push after reset: %v", err)
	}
}

func TestPeakNeverExceedsLimitWithConcurrentProducers(t *testing.T) {
	const limit = 256
	q := NewQueue(limit)
	var wg sync.WaitGroup
	var pushes atomic.Int64
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				j := Job{ID: int(pushes.Add(1)), Cost: 64}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := q.Push(ctx, &j)
				cancel()
				if err != nil {
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := q.Pop(context.Background()); err != nil {
				return
			}
		}
	}()
	wg.Wait()
	q.Close()
	<-done
	if q.Peak() > limit {
		t.Fatalf("peak %d > limit %d", q.Peak(), limit)
	}
	if q.Used() > limit {
		t.Fatalf("final usage %d > limit", q.Used())
	}
}

func TestMonitorQueueMemory(t *testing.T) {
	// Sanity: the exported limit is the documented 64 MiB.
	if MemoryLimit != 64*1024*1024 {
		t.Fatalf("MemoryLimit changed: %d", MemoryLimit)
	}
	q := NewQueue(0)
	if q.Limit() != MemoryLimit {
		t.Fatalf("default limit %d", q.Limit())
	}
	job := Job{ID: 0, Cost: MemoryLimit}
	if err := q.Push(context.Background(), &job); err != nil {
		t.Fatalf("full-budget job rejected: %v", err)
	}
	q.Reset()
	if q.Used() != 0 {
		t.Fatalf("reset left %d bytes", q.Used())
	}
}
