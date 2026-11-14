package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStateTransitions(t *testing.T) {
	if err := CheckTransition(Pending, Probing); err != nil {
		t.Fatal(err)
	}
	if err := CheckTransition(Encoding, Done); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("got %v, want illegal transition", err)
	}
	if err := CheckTransition(Done, Pending); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("got %v, want illegal terminal transition", err)
	}
}

func TestJournalTruncatesPartialTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.ndjson")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Transition(1, Probing, map[string]any{"attempt": 0}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	j2, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if got := j2.State(1); got != Probing {
		t.Fatalf("state=%s, want PROBING", got)
	}
	if err := j2.Transition(1, Planning, nil); err != nil {
		t.Fatal(err)
	}
	f2, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.WriteString(`{"seq":`); err != nil {
		t.Fatal(err)
	}
	_ = f2.Close()
	j3, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = j3.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("partial journal tail was not discarded")
	}
}

func TestSchedulerRetries(t *testing.T) {
	var attempts atomic.Int32
	s := Scheduler{Concurrency: 1, MaxRetries: 2, BackoffBase: time.Millisecond, MaxBackoff: 5 * time.Millisecond}
	err := s.Run(context.Background(), []Task{{ID: 1, Run: func(ctx context.Context, attempt int) error {
		attempts.Add(1)
		if attempt < 2 {
			return errors.New("transient")
		}
		return nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts=%d, want 3", got)
	}
}

func TestJournalConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.ndjson")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := j.Record("progress", i%4, map[string]int{"worker": i}); err != nil {
				t.Errorf("record: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestSchedulerSuccessLimitAndCancellation(t *testing.T) {
	var done atomic.Int32
	s := Scheduler{Concurrency: 1, SuccessLimit: 2, BackoffBase: time.Millisecond}
	tasks := make([]Task, 5)
	for i := range tasks {
		tasks[i] = Task{ID: i + 1, Run: func(ctx context.Context, attempt int) error {
			done.Add(1)
			return nil
		}}
	}
	if err := s.Run(context.Background(), tasks); !errors.Is(err, ErrSuccessLimit) {
		t.Fatalf("got %v, want ErrSuccessLimit", err)
	}
	if got := done.Load(); got != 2 {
		t.Fatalf("successes=%d, want 2", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := s.Run(ctx, []Task{{ID: 99, Run: func(ctx context.Context, attempt int) error {
		<-ctx.Done()
		return ctx.Err()
	}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
