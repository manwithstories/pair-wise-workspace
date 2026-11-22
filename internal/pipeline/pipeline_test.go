package pipeline

import (
	"context"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"medialine/internal/fault"
)

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestPoolRunsAllJobs(t *testing.T) {
	p, err := New(context.Background(), Config{Workers: 3, Queue: 4})
	if err != nil {
		t.Fatal(err)
	}
	var done int64
	var wg sync.WaitGroup
	wg.Add(20)
	for i := 0; i < 20; i++ {
		if _, err := p.Submit(func(ctx context.Context) error { return nil }, func(_ int64, err error) {
			if err != nil {
				t.Errorf("unexpected: %v", err)
			}
			atomic.AddInt64(&done, 1)
			wg.Done()
		}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	p.Close()
	if got := atomic.LoadInt64(&done); got != 20 {
		t.Fatalf("done=%d", got)
	}
	st := p.Stats()
	if st.Accepted != 20 || st.Failed != 0 {
		t.Fatalf("stats=%+v", st)
	}
	if st.Peak > st.Limit() {
		t.Fatalf("peak %d exceeds structural limit %d", st.Peak, st.Limit())
	}
}

func TestPoolRejectsBadConfig(t *testing.T) {
	if _, err := New(context.Background(), Config{Workers: 0}); err == nil {
		t.Fatal("expected worker error")
	}
	if _, err := New(context.Background(), Config{Workers: 1, Queue: -1}); err == nil {
		t.Fatal("expected queue error")
	}
}

func TestPoolPeakIsBounded(t *testing.T) {
	p, err := New(context.Background(), Config{Workers: 2, Queue: 3})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(30)
	for i := 0; i < 30; i++ {
		// Submission must block once queue+workers jobs are outstanding; the
		// release channel unblocks them so the producer cannot outrun us.
		go func() {
			p.Submit(func(ctx context.Context) error {
				<-release
				return nil
			}, func(_ int64, err error) {
				wg.Done()
			})
		}()
	}
	eventually(t, func() bool { return p.Stats().Peak >= 2 })
	close(release)
	wg.Wait()
	p.Close()
	st := p.Stats()
	if st.Peak > st.Limit() {
		t.Fatalf("peak %d > limit %d", st.Peak, st.Limit())
	}
}

func TestPoolCancelDrains(t *testing.T) {
	p, err := New(context.Background(), Config{Workers: 1, Queue: 8})
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	var ran, cancelled int64
	p.Submit(func(ctx context.Context) error { <-gate; return nil }, func(_ int64, err error) {})
	for i := 0; i < 5; i++ {
		p.Submit(func(ctx context.Context) error { atomic.AddInt64(&ran, 1); return nil }, func(_ int64, err error) {
			if err != nil {
				atomic.AddInt64(&cancelled, 1)
			}
		})
	}
	p.Cancel()
	close(gate)
	p.Close()
	if atomic.LoadInt64(&cancelled) == 0 {
		t.Fatal("queued jobs should report cancellation")
	}
	if _, err := p.Submit(func(context.Context) error { return nil }, nil); err == nil {
		t.Fatal("submit after cancel should fail")
	}
}

func TestPoolWorkerPanicIsContained(t *testing.T) {
	var got *fault.Error
	p, err := New(context.Background(), Config{Workers: 1, Queue: 1, OnFault: func(e *fault.Error) { got = e }})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	p.Submit(func(ctx context.Context) error { panic("boom") }, func(_ int64, err error) { done <- err })
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected panic error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("panic job never completed")
	}
	p.Close()
	if got == nil {
		t.Fatal("fault handler not called")
	}
}

func TestMergerOrdersDeterministically(t *testing.T) {
	m := NewMerger[int](context.Background())
	m.Add(Result[int]{Key: MergeKey{StartUS: 2000, TrackID: 1, RepID: "b"}, Value: 3})
	m.Add(Result[int]{Key: MergeKey{StartUS: 1000, TrackID: 2, RepID: "a"}, Value: 1})
	m.Add(Result[int]{Key: MergeKey{StartUS: 1000, TrackID: 1, RepID: "z"}, Value: 2})
	ordered := m.Ordered()
	want := []int{2, 1, 3}
	for i, w := range want {
		if ordered[i].Value != w {
			t.Fatalf("index %d=%d want %d", i, ordered[i].Value, w)
		}
	}
}

func TestMergerDedup(t *testing.T) {
	m := NewMerger[string](context.Background())
	k := MergeKey{StartUS: 0, TrackID: 1, RepID: "a"}
	m.Add(Result[string]{Key: k, Value: "first"})
	m.Add(Result[string]{Key: k, Value: "second"})
	m.Add(Result[string]{Key: MergeKey{StartUS: 1000, TrackID: 1, RepID: "a"}, Value: "other"})
	if removed := m.Dedup(); removed != 1 {
		t.Fatalf("removed=%d", removed)
	}
	ordered := m.Ordered()
	if len(ordered) != 2 || ordered[0].Value != "first" {
		t.Fatalf("ordered=%+v", ordered)
	}
}

func TestMergerEmitStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewMerger[int](ctx)
	for i := 0; i < 10; i++ {
		m.Add(Result[int]{Key: MergeKey{StartUS: int64(i)}, Value: i})
	}
	cancel()
	ch := make(chan Result[int])
	if err := m.Emit(ctx, ch); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestMergerEmitDelivers(t *testing.T) {
	ctx := context.Background()
	m := NewMerger[int](ctx)
	m.Add(Result[int]{Key: MergeKey{StartUS: 0}, Value: 1})
	m.Add(Result[int]{Key: MergeKey{StartUS: 1}, Value: 2})
	ch := make(chan Result[int], 2)
	if err := m.Emit(ctx, ch); err != nil {
		t.Fatal(err)
	}
	close(ch)
	var got []int
	for r := range ch {
		got = append(got, r.Value)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("got=%v", got)
	}
}

func TestMergeKeyLess(t *testing.T) {
	a := MergeKey{StartUS: 1, TrackID: 1, RepID: "a", Seq: 1}
	b := MergeKey{StartUS: 1, TrackID: 1, RepID: "a", Seq: 2}
	if !a.Less(b) || b.Less(a) {
		t.Fatal("seq should break ties")
	}
	c := MergeKey{StartUS: 2}
	if !a.Less(c) || c.Less(a) {
		t.Fatal("start should dominate")
	}
}

func TestCancellerReasons(t *testing.T) {
	c := NewCanceller(context.Background())
	if c.Canceled() || c.Err() != nil {
		t.Fatal("fresh canceller should not be cancelled")
	}
	c.CancelOnError(nil)
	if c.Canceled() {
		t.Fatal("nil error must not cancel")
	}
	c.CancelOnError(fault.New(fault.StageParse, fault.KindTruncatedBox, "recoverable"))
	if c.Canceled() {
		t.Fatal("recoverable fault must not cancel")
	}
	c.CancelOnError(fault.New(fault.StageWrite, fault.KindWrite, "fatal"))
	if !c.Canceled() || c.Reason() != ReasonFault {
		t.Fatalf("reason=%s", c.Reason())
	}
	c.Cancel(ReasonSignal)
	if c.Reason() != ReasonFault {
		t.Fatal("first reason wins")
	}
	if err := c.Err(); !fault.IsCanceled(err) {
		t.Fatalf("err=%v", err)
	}
}

func TestGuardCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeCloser{}
	g := NewGuard(ctx, f)
	if _, err := g.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := g.Write([]byte("y")); err == nil {
		t.Fatal("expected cancelled write")
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if !f.closed {
		t.Fatal("underlying writer not closed")
	}
	if _, err := g.Write([]byte("z")); err == nil {
		t.Fatal("write after close should fail")
	}
}

func TestRecoverableOrCanceled(t *testing.T) {
	if RecoverableOrCanceled(nil) {
		t.Fatal("nil is not fatal")
	}
	if RecoverableOrCanceled(fault.New(fault.StageParse, fault.KindCorruptSample, "x")) {
		t.Fatal("recoverable fault should not stop")
	}
	if !RecoverableOrCanceled(fault.New(fault.StageWrite, fault.KindWrite, "x")) {
		t.Fatal("write fault should stop")
	}
	if !RecoverableOrCanceled(context.Canceled) {
		t.Fatal("context cancellation should stop")
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	p, _ := New(context.Background(), Config{Workers: 4, Queue: 4})
	for i := 0; i < 50; i++ {
		p.Submit(func(ctx context.Context) error { return nil }, nil)
	}
	p.Cancel()
	p.Close()
	eventually(t, func() bool { return runtime.NumGoroutine() <= before+1 })
}

type fakeCloser struct {
	closed bool
	err    error
}

func (f *fakeCloser) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeCloser) Close() error                { f.closed = true; return f.err }

var _ io.WriteCloser = (*fakeCloser)(nil)
