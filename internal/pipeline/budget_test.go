package pipeline

import (
	"testing"

	"mediad/internal/codec"
	"mediad/internal/timeline"
)

func TestByteBudgetTracksAndRejectsOversizedAU(t *testing.T) {
	b := newByteBudget(10)
	if err := b.acquire(4); err != nil {
		t.Fatal(err)
	}
	used, _ := b.stats()
	if used != 4 {
		t.Fatalf("used=%d want 4", used)
	}
	b.release(4)
	used, _ = b.stats()
	if used != 0 {
		t.Fatalf("used=%d after release", used)
	}

	err := b.acquire(codec.MaxAUBytes + 1)
	if code := timeline.ExitCode(err); code != 2 {
		t.Fatalf("oversized AU code=%d want 2 err=%v", code, err)
	}
}

func TestMemoryBudgetMatchesEdgeConstraint(t *testing.T) {
	if MemoryLimitBytes != 64<<20 {
		t.Fatalf("memory limit=%d want 64 MiB", MemoryLimitBytes)
	}
	if codec.MaxAUBytes != 8<<20 {
		t.Fatalf("AU limit=%d want 8 MiB", codec.MaxAUBytes)
	}
}
