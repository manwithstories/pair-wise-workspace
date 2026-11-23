package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mediascan/internal/fault"
)

func TestPoolRunsAllJobs(t *testing.T) {
	p, err := NewPool(Limits{MaxQueueLen: 16, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	var done int64
	jobs := make([]Job, 20)
	for i := range jobs {
		i := i
		jobs[i] = Job{Seq: int64(i), SegmentID: int64(i / 5), Work: func(context.Context) error {
			atomic.AddInt64(&done, 1)
			return nil
		}}
	}
	if err := p.Run(context.Background(), jobs); err != nil {
		t.Fatalf("run: %v", err)
	}
	if done != 20 {
		t.Fatalf("done=%d want 20", done)
	}
}

func TestPoolConcurrencyBounded(t *testing.T) {
	p, _ := NewPool(Limits{MaxQueueLen: 64, MaxConcurrent: 3})
	var inFlight, maxSeen int64
	var mu sync.Mutex
	jobs := make([]Job, 30)
	for i := range jobs {
		jobs[i] = Job{Seq: int64(i), Work: func(context.Context) error {
			cur := atomic.AddInt64(&inFlight, 1)
			mu.Lock()
			if cur > maxSeen {
				maxSeen = cur
			}
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
			return nil
		}}
	}
	if err := p.Run(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if maxSeen > 3 {
		t.Fatalf("max concurrency=%d exceeds 3", maxSeen)
	}
}

func TestPoolJobFailureStopsProduction(t *testing.T) {
	p, _ := NewPool(Limits{MaxQueueLen: 8, MaxConcurrent: 2})
	boom := errors.New("boom")
	var started int64
	jobs := make([]Job, 12)
	for i := range jobs {
		i := i
		jobs[i] = Job{Seq: int64(i), Work: func(context.Context) error {
			atomic.AddInt64(&started, 1)
			if i == 3 {
				return boom
			}
			time.Sleep(5 * time.Millisecond)
			return nil
		}}
	}
	err := p.Run(context.Background(), jobs)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v want boom", err)
	}
	// Production must stop well before dispatching all 12 jobs.
	if got := atomic.LoadInt64(&started); got >= int64(len(jobs)) {
		t.Fatalf("production did not stop: started=%d", got)
	}
}

func TestPoolCancellation(t *testing.T) {
	p, _ := NewPool(Limits{MaxQueueLen: 4, MaxConcurrent: 2})
	ctx, cancel := context.WithCancel(context.Background())
	var ran int64
	block := make(chan struct{})
	jobs := make([]Job, 10)
	for i := range jobs {
		jobs[i] = Job{Seq: int64(i), Work: func(c context.Context) error {
			atomic.AddInt64(&ran, 1)
			select {
			case <-c.Done():
				return c.Err()
			case <-block:
				return nil
			}
		}}
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := p.Run(ctx, jobs)
	close(block)
	if !errors.Is(err, ErrPoolCanceled) && !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want cancellation", err)
	}
}

func TestPoolQueueStaysBounded(t *testing.T) {
	// A small buffer must still process a large total number of jobs,
	// proving the queue is a backpressure bound, not a total-job cap.
	p, _ := NewPool(Limits{MaxQueueLen: 2, MaxConcurrent: 2})
	var done int64
	jobs := make([]Job, 50)
	for i := range jobs {
		jobs[i] = Job{Seq: int64(i), Work: func(context.Context) error {
			atomic.AddInt64(&done, 1)
			return nil
		}}
	}
	if err := p.Run(context.Background(), jobs); err != nil {
		t.Fatalf("run: %v", err)
	}
	if done != 50 {
		t.Fatalf("done=%d want 50", done)
	}
	if _, err := NewPool(Limits{MaxQueueLen: 0, MaxConcurrent: 1}); err == nil {
		t.Fatal("invalid limits must be rejected")
	}
	if _, err := NewPool(Limits{MaxQueueLen: 4, MaxConcurrent: 0}); err == nil {
		t.Fatal("invalid concurrency must be rejected")
	}
}

func TestMergeFaultsDeterministic(t *testing.T) {
	mk := func(ticks int64, track uint32, off int64, kind fault.Kind) fault.Fault {
		return fault.Fault{Kind: kind, TrackID: track, Ticks: ticks, OffsetBytes: off, FrameIndex: 0}
	}
	a := []fault.Fault{mk(300, 2, 10, fault.KindBadSample), mk(100, 1, 5, fault.KindKeyframeGap)}
	b := []fault.Fault{mk(100, 1, 4, fault.KindTruncatedBox), mk(200, 0, 9, fault.KindRTPSeqGap)}
	merged := MergeFaults(a, b)
	if len(merged) != 4 {
		t.Fatalf("len=%d", len(merged))
	}
	// Expected order: ticks 100 (offset 4 then 5), 200, 300.
	want := []struct {
		ticks int64
		off   int64
	}{{100, 4}, {100, 5}, {200, 9}, {300, 10}}
	for i, w := range want {
		if merged[i].Ticks != w.ticks || merged[i].OffsetBytes != w.off {
			t.Fatalf("position %d = (%d,%d) want (%d,%d)",
				i, merged[i].Ticks, merged[i].OffsetBytes, w.ticks, w.off)
		}
	}
	// Shuffle the input partials; output must stay identical.
	merged2 := MergeFaults(b, a)
	for i := range merged {
		if fault.OrderKey(merged[i], merged2[i]) != 0 {
			t.Fatalf("merge not order-stable at %d", i)
		}
	}
}

type sampleResult struct {
	ticks  int64
	track  uint32
	offset int64
	tag    string
}

func (s sampleResult) PositionTicks() int64  { return s.ticks }
func (s sampleResult) PositionTrack() uint32 { return s.track }
func (s sampleResult) PositionOffset() int64 { return s.offset }

func TestMergeResultsStable(t *testing.T) {
	p1 := []sampleResult{{300, 2, 1, "c"}, {100, 1, 1, "a"}}
	p2 := []sampleResult{{100, 1, 2, "b"}, {50, 0, 9, "z"}}
	merged := MergeResults(p1, p2)
	wantTags := []string{"z", "a", "b", "c"}
	for i, r := range merged {
		if r.tag != wantTags[i] {
			t.Fatalf("pos %d tag=%s want %s (%+v)", i, r.tag, wantTags[i], merged)
		}
	}
	if fmt.Sprint(len(merged)) == "0" {
		t.Fatal("merge empty check")
	}
	if got := MergeResults[sampleResult](); len(got) != 0 {
		t.Fatalf("empty merge must return empty non-nil slice")
	}
}
