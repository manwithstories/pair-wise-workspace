package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/timeline"
)

// entries builds n timeline entries with deliberately interleaved tracks and
// shuffled start times, so a naive completion-order merge would differ.
func entries(n int) []timeline.Entry {
	out := make([]timeline.Entry, n)
	for i := 0; i < n; i++ {
		out[i] = timeline.Entry{
			Track:     i % 3,
			SampleIdx: (n - i) % 7,
			Start90k:  int64(n - i),
			Dur90k:    1,
		}
	}
	return out
}

func TestPoolRejectsInvalidConfig(t *testing.T) {
	if _, err := New(context.Background(), Config{Workers: 0, Inflight: 1}); err == nil {
		t.Fatal("zero workers must be rejected")
	}
	if _, err := New(context.Background(), Config{Workers: 1, Inflight: -1}); err == nil {
		t.Fatal("negative inflight must be rejected")
	}
	p, err := New(context.Background(), Config{Workers: 2, Inflight: 0})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if p.Inflight() != 2 {
		t.Fatalf("inflight default = %d, want workers", p.Inflight())
	}
	p.Close()
}

func TestMergeOrderIsDeterministic(t *testing.T) {
	// Run the same work many times with varying completion order and require
	// identical merged output every time.
	items := entries(40)
	var first []int
	for trial := 0; trial < 8; trial++ {
		if trial%2 == 1 {
			// Alternate the input order; the merge key is independent of it.
			for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
				items[i], items[j] = items[j], items[i]
			}
		}
		var got []int
		res, err := Run(context.Background(), Config{Workers: 4, Inflight: 3}, RunOptions{
			Items: items,
			Work: func(ctx context.Context, e timeline.Entry) (any, error) {
				// Random-ish delays would make completion order vary; a short
				// sleep based on the index is enough under -race.
				if e.SampleIdx%3 == 0 {
					time.Sleep(time.Millisecond)
				}
				return e.SampleIdx, nil
			},
			OnResult: func(m Merged) error {
				got = append(got, m.Entry.SampleIdx)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if res.Merged != len(items) {
			t.Fatalf("merged = %d, want %d", res.Merged, len(items))
		}
		if trial == 0 {
			first = append([]int(nil), got...)
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("trial %d length = %d, want %d", trial, len(got), len(first))
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("trial %d differs at %d: %d vs %d", trial, i, got[i], first[i])
			}
		}
	}
}

func TestRunCancellationStopsBeforeEmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	items := entries(200)
	var mu sync.Mutex
	var emitted int
	go func() {
		time.Sleep(3 * time.Millisecond)
		cancel()
	}()
	res, err := Run(ctx, Config{Workers: 4, Inflight: 4}, RunOptions{
		Items: items,
		Work: func(ctx context.Context, e timeline.Entry) (any, error) {
			select {
			case <-ctx.Done():
				return nil, fault.Wrap(-1, 0, fault.KindCancelled, true, ctx.Err())
			case <-time.After(time.Millisecond):
			}
			return e.SampleIdx, nil
		},
		OnResult: func(m Merged) error {
			mu.Lock()
			emitted++
			mu.Unlock()
			return nil
		},
	})
	mu.Lock()
	defer mu.Unlock()
	if emitted != 0 {
		t.Fatalf("cancelled run emitted %d rows; nothing may be written", emitted)
	}
	if !res.Canceled {
		t.Fatalf("result not marked cancelled: %+v", res)
	}
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCancelled || !fe.Recoverable {
		t.Fatalf("expected recoverable cancelled fault, got %v", err)
	}
}

func TestRunJobErrorsDoNotAbort(t *testing.T) {
	items := entries(10)
	var mu sync.Mutex
	var failures int
	res, err := Run(context.Background(), Config{Workers: 3, Inflight: 2}, RunOptions{
		Items: items,
		Work: func(ctx context.Context, e timeline.Entry) (any, error) {
			if e.Track == 0 {
				return nil, fault.New(0, 0, fault.KindCorrupt, true, "boom %d", e.SampleIdx)
			}
			return e.SampleIdx, nil
		},
		OnError: func(err error) {
			mu.Lock()
			failures++
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if failures == 0 {
		t.Fatal("expected per-item failures to be reported")
	}
	if res.Merged+failures != len(items) {
		t.Fatalf("merged %d + failed %d != %d", res.Merged, failures, len(items))
	}
	if res.Faults.Len() != failures {
		t.Fatalf("fault list = %d, want %d", res.Faults.Len(), failures)
	}
}

func TestRunRecoversPanics(t *testing.T) {
	_, err := Run(context.Background(), Config{Workers: 2, Inflight: 2}, RunOptions{
		Items: entries(2),
		Work: func(ctx context.Context, e timeline.Entry) (any, error) {
			panic("kaboom")
		},
		OnError: func(error) {},
	})
	if err != nil {
		t.Fatalf("panic must not escape the pool: %v", err)
	}
}

func TestPoolSubmitAfterCloseFails(t *testing.T) {
	p, err := New(context.Background(), Config{Workers: 1, Inflight: 1})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	p.Close()
	if p.Submit(Job{Run: func(context.Context) (any, error) { return nil, nil }}) {
		t.Fatal("submit after close must fail")
	}
	// Close is idempotent.
	p.Close()
}

func TestMergeSortKey(t *testing.T) {
	m := NewMerge()
	m.Add(3, timeline.Entry{Start90k: 10, Track: 2, SampleIdx: 0}, nil)
	m.Add(1, timeline.Entry{Start90k: 10, Track: 1, SampleIdx: 5}, nil)
	m.Add(2, timeline.Entry{Start90k: 10, Track: 1, SampleIdx: 2}, nil)
	got := m.Sorted()
	want := []int{2, 1, 3}
	for i, w := range want {
		if got[i].ID != w {
			t.Fatalf("order = %v, want %v", ids(got), want)
		}
	}
}

func ids(ms []Merged) []int {
	out := make([]int, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestRunPropagatesContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Config{Workers: 2, Inflight: 2}, RunOptions{
		Items: entries(5),
		Work:  func(context.Context, timeline.Entry) (any, error) { return nil, nil },
	})
	if err == nil {
		t.Fatal("a pre-cancelled context must not report success")
	}
	if !errors.Is(err, err) || !fault.Recoverable(err) {
		t.Fatalf("expected recoverable cancelled fault, got %v", err)
	}
}
