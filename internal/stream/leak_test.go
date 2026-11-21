package stream

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"mediamerge/internal/fault"
)

// TestNoGoroutineLeakAfterRepeatedFailures runs many failing pipelines and
// checks that the goroutine count returns to its baseline, which is the
// contract for "release every blocked goroutine".
func TestNoGoroutineLeakAfterRepeatedFailures(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for i := 0; i < 40; i++ {
		q, err := NewQueue(1024)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		boom := fault.New(fault.StageStream, fault.CodeWriteFailed, "boom")
		sink, err := RunPipeWith(ctx, q, func(pctx context.Context) error {
			for n := 0; n < 50; n++ {
				if err := q.Push(pctx, block(n+1, 1, 200)); err != nil {
					return err
				}
				if n == 5 {
					return boom
				}
			}
			return nil
		}, func(cctx context.Context) (*Sink, error) {
			return Consume(cctx, q, ConsumerOptions{Delay: time.Millisecond})
		}, ConsumerOptions{})
		cancel()
		if err == nil || sink == nil {
			t.Fatalf("iteration %d: err=%v sink=%v", i, err, sink)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		now := runtime.NumGoroutine()
		if now <= baseline+2 {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutines grew from %d to %d\n%s", baseline, now, buf[:n])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestQueueBudgetAccounting ensures the reported occupancy is exact across
// many pushes and pops.
func TestQueueBudgetAccounting(t *testing.T) {
	q, _ := NewQueue(1 << 20)
	ctx := context.Background()
	for round := 0; round < 100; round++ {
		for i := 0; i < 7; i++ {
			if err := q.Push(ctx, block(round*7+i, 1, 100)); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 7; i++ {
			if _, err := q.Pop(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if st := q.Stats(); st.Bytes != 0 || st.Items != 0 {
			t.Fatalf("round %d left %+v", round, st)
		}
	}
	if fmt.Sprint(q.Stats().Budget) != "1048576" {
		t.Fatalf("budget changed: %+v", q.Stats())
	}
}
