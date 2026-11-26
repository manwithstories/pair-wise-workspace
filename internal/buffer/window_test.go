package buffer

import (
	"context"
	"sync"
	"testing"
	"time"

	"mediamix/internal/clock"
)

func TestNewWindowRejectsBadParams(t *testing.T) {
	cases := []struct{ items, bytes int }{
		{0, 1024}, {-1, 1024}, {8, 0}, {8, -1}, {8, MaxQueueBytes + 1},
	}
	for _, c := range cases {
		if _, err := NewWindow[[]byte](c.items, c.bytes, func(b []byte) int { return len(b) }); err == nil {
			t.Fatalf("items=%d bytes=%d should be rejected", c.items, c.bytes)
		} else if !clock.IsCode(err, clock.ErrInvalidParam) {
			t.Fatalf("expected ErrInvalidParam, got %v", err)
		}
	}
}

func TestFIFOAndPeakCap(t *testing.T) {
	const capBytes = 64
	w, err := NewWindow[[]byte](4, capBytes, func(b []byte) int { return len(b) })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := w.Put(ctx, []byte{byte(i), byte(i)}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if got := w.PeakBytes(); got > capBytes {
		t.Fatalf("peak %d exceeds cap %d", got, capBytes)
	}
	if err := w.Put(ctx, make([]byte, capBytes+1)); !clock.IsCode(err, clock.ErrLimitExceeded) {
		t.Fatalf("oversized item should be rejected, got %v", err)
	}
	for i := 0; i < 4; i++ {
		v, err := w.Get(ctx)
		if err != nil || len(v) != 2 || v[0] != byte(i) {
			t.Fatalf("get %d: v=%v err=%v", i, v, err)
		}
	}
}

func TestBackpressureBlocksAndWakes(t *testing.T) {
	w, _ := NewWindow[int](2, 10, func(int) int { return 4 })
	ctx := context.Background()
	if err := w.Put(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := w.Put(ctx, 2); err != nil {
		t.Fatal(err)
	}
	putDone := make(chan error, 1)
	go func() { putDone <- w.Put(ctx, 3) }()
	select {
	case <-putDone:
		t.Fatal("producer should block on full queue")
	case <-time.After(30 * time.Millisecond):
	}
	if _, err := w.Get(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-putDone:
		if err != nil {
			t.Fatalf("blocked put should resume: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer never woke after drain")
	}
}

func TestCancelReleasesWaiters(t *testing.T) {
	w, _ := NewWindow[int](1, 10, func(int) int { return 4 })
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Put(ctx, 1); err != nil {
		t.Fatal(err)
	}
	putErr := make(chan error, 1)
	go func() { putErr <- w.Put(ctx, 2) }()
	time.Sleep(20 * time.Millisecond)

	emptyW, _ := NewWindow[int](1, 10, func(int) int { return 4 })
	getErr := make(chan error, 1)
	go func() { _, err := emptyW.Get(ctx); getErr <- err }()
	time.Sleep(20 * time.Millisecond)

	cancel()
	for name, ch := range map[string]chan error{"put": putErr, "get": getErr} {
		select {
		case err := <-ch:
			if !clock.IsCode(err, clock.ErrTimeout) {
				t.Fatalf("%s: expected timeout error, got %v", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s waiter not released by cancel", name)
		}
	}
}

func TestCloseReleasesWaitersAndDrains(t *testing.T) {
	w, _ := NewWindow[int](2, 10, func(int) int { return 4 })
	ctx := context.Background()
	_ = w.Put(ctx, 7)

	getErr := make(chan error, 1)
	go func() {
		_, err := w.Get(ctx) // gets 7
		getErr <- err
		_, err = w.Get(ctx) // blocks on empty
		getErr <- err
	}()
	putErr := make(chan error, 1)
	full, _ := NewWindow[int](1, 4, func(int) int { return 4 })
	_ = full.Put(ctx, 1)
	go func() { putErr <- full.Put(ctx, 2) }()

	time.Sleep(20 * time.Millisecond)
	w.Close()
	full.Close()

	if err := <-getErr; err != nil {
		t.Fatalf("first get after close should drain: %v", err)
	}
	if err := <-getErr; !clock.IsCode(err, clock.ErrQueueClosed) {
		t.Fatalf("drained get should report closed, got %v", err)
	}
	if err := <-putErr; !clock.IsCode(err, clock.ErrQueueClosed) {
		t.Fatalf("blocked put should report closed, got %v", err)
	}
	if err := w.Put(ctx, 9); !clock.IsCode(err, clock.ErrQueueClosed) {
		t.Fatalf("put after close must fail, got %v", err)
	}
}

func TestPeakNeverExceedsBudget(t *testing.T) {
	const budget = 4096
	w, _ := NewWindow[[]byte](100000, budget, func(b []byte) int { return len(b) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if err := w.Put(ctx, make([]byte, 128)); err != nil {
					return
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if _, err := w.Get(ctx); err != nil {
				return
			}
		}
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	wg.Wait()
	if got := w.PeakBytes(); got > budget {
		t.Fatalf("peak %d exceeded budget %d", got, budget)
	}
}
