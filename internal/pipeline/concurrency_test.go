package pipeline

import (
	"context"
	"testing"
	"time"
)

func TestTaskLimiterBoundsConcurrency(t *testing.T) {
	l := NewTaskLimiter(2)
	if !l.TryAcquire() || !l.TryAcquire() {
		t.Fatal("first two acquisitions should succeed")
	}
	if l.TryAcquire() {
		t.Fatal("third acquisition must be rejected at limit")
	}
	l.Release()
	if !l.TryAcquire() {
		t.Fatal("release should permit another task")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.Acquire(ctx); err == nil {
		t.Fatal("acquire should block/fail while both slots are held")
	}
}

func TestTaskLimiterClampsToCeiling(t *testing.T) {
	l := NewTaskLimiter(10_000)
	if cap(l) != MaxConcurrentTasks {
		t.Fatalf("capacity=%d want clamp %d", cap(l), MaxConcurrentTasks)
	}
}
