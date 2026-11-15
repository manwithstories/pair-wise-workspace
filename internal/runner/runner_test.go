package runner

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"rcctl/internal/plan"
	"rcctl/internal/state"
)

func TestPlanAndExecuteSegments(t *testing.T) {
	p := plan.New("unit", 12, 32, 32, 30, 4, 300, 9)
	if err := BuildPlan(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(p.Segments) != 3 {
		t.Fatalf("got %d segments", len(p.Segments))
	}
	journalPath := filepath.Join(t.TempDir(), "journal.log")
	j, err := state.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	results, failed, err := Execute(context.Background(), p, nil, j, Options{
		Workers: 2, MaxAttempts: 2, Backoff: time.Millisecond, RatePasses: 3, RateTolerance: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	if len(failed) != 0 || len(results) != 3 {
		t.Fatalf("results=%d failed=%v", len(results), failed)
	}
	for _, result := range results {
		if result.Status != state.Done || result.Bytes <= 0 || result.PSNRDB <= 0 {
			t.Fatalf("invalid result: %#v", result)
		}
	}
	snapshot, err := state.Read(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Completed) != 3 || len(snapshot.Warnings) != 0 {
		t.Fatalf("journal replay: %#v", snapshot)
	}
}
