package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mediaproc/internal/model"
)

func TestQueueCapacityAndPeak(t *testing.T) {
	r := NewRunner(2, 1<<20, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(_ context.Context, _ Item) error {
			started <- struct{}{}
			<-release
			return nil
		})
	}()

	for i := 0; i < 3; i++ {
		if err := r.Submit(ctx, Item{Offset: int64(i), Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("consumer did not start")
	}
	// One item is in the consumer and two are buffered: peak is capacity two.
	if m := r.Metrics(); m.PeakQueue != 2 {
		t.Fatalf("peak queue=%d want 2", m.PeakQueue)
	}
	close(release)
	r.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFullQueueCancelReportsOffsetAndCleans(t *testing.T) {
	r := NewRunner(1, 1<<20, 0)
	runnerCtx, cancelRunner := context.WithCancel(context.Background())
	started := make(chan struct{})
	blockConsumer := make(chan struct{})
	var cleanMu sync.Mutex
	var cleaned []int64
	r.Cleanup = func(item Item) {
		cleanMu.Lock()
		cleaned = append(cleaned, item.Offset)
		cleanMu.Unlock()
	}

	runDone := make(chan error, 1)
	go func() {
		runDone <- r.Run(runnerCtx, func(ctx context.Context, _ Item) error {
			close(started)
			select {
			case <-blockConsumer:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	if err := r.Submit(context.Background(), Item{Offset: 10, Size: 1}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := r.Submit(context.Background(), Item{Offset: 20, Size: 1}); err != nil {
		t.Fatal(err)
	}

	submitCtx, cancelSubmit := context.WithCancel(context.Background())
	submitErr := make(chan error, 1)
	go func() {
		submitErr <- r.Submit(submitCtx, Item{Offset: 99, Size: 1})
	}()
	time.Sleep(20 * time.Millisecond) // let the third submit block on the full queue
	cancelSubmit()
	var fe *model.Error
	if err := <-submitErr; !errors.As(err, &fe) || fe.Offset != 99 {
		t.Fatalf("submit error=%v want offset 99", err)
	}

	cancelRunner()
	close(blockConsumer)
	err := <-runDone
	if !errors.As(err, &fe) || fe.Offset != 99 {
		t.Fatalf("run error=%v want cancel offset 99", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		cleanMu.Lock()
		n := len(cleaned)
		cleanMu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cleanMu.Lock()
	defer cleanMu.Unlock()
	if len(cleaned) != 2 || cleaned[0] != 10 || cleaned[1] != 20 {
		t.Fatalf("cleaned=%v want in-flight offset 10 and queued offset 20", cleaned)
	}
}

func TestMemoryBudgetBackpressureAndOversize(t *testing.T) {
	r := NewRunner(4, 10, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Submit(ctx, Item{Offset: 1, Size: 11}); err == nil {
		t.Fatal("expected oversize item fault")
	} else if fe, ok := model.AsError(err); !ok || fe.Offset != 1 {
		t.Fatalf("oversize error=%v", err)
	}

	if err := r.Submit(ctx, Item{Offset: 2, Size: 6}); err != nil {
		t.Fatal(err)
	}
	if err := r.Submit(ctx, Item{Offset: 3, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if m := r.Metrics(); m.PeakMemory > 10 {
		t.Fatalf("peak memory=%d exceeds budget", m.PeakMemory)
	}
}
