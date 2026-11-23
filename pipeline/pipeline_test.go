package pipeline_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bcast/media"
	"bcast/pipeline"
)

func TestDependencyOrderAndConcurrency(t *testing.T) {
	var mu sync.Mutex
	var order []string
	rec := func(id string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) {
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
			return id, nil
		}
	}
	d := pipeline.New(nil)
	_ = d.Add(pipeline.Node{ID: "a", Run: rec("a")})
	_ = d.Add(pipeline.Node{ID: "b", Deps: []string{"a"}, Run: rec("b")})
	_ = d.Add(pipeline.Node{ID: "c", Deps: []string{"a"}, Run: rec("c")})
	rep := d.Execute(context.Background(), 4)
	if !rep.OK() {
		t.Fatalf("report=%+v", rep)
	}
	if order[0] != "a" {
		t.Fatalf("a must run first: %v", order)
	}
	if rep.DoneNodes != 3 {
		t.Fatalf("done=%d", rep.DoneNodes)
	}
}

func TestFailurePropagatesToRootCause(t *testing.T) {
	d := pipeline.New(nil)
	_ = d.Add(pipeline.Node{ID: "demux", Run: func(context.Context) (string, error) {
		return "", media.Fatal("demux.parse", media.ErrMissingMoov)
	}})
	_ = d.Add(pipeline.Node{ID: "index", Deps: []string{"demux"}, Run: func(context.Context) (string, error) {
		t.Fatal("index must not run")
		return "", nil
	}})
	_ = d.Add(pipeline.Node{ID: "hash", Deps: []string{"index"}, Run: func(context.Context) (string, error) {
		t.Fatal("hash must not run")
		return "", nil
	}})
	rep := d.Execute(context.Background(), 2)
	if rep.OK() {
		t.Fatal("expected failure")
	}
	if len(rep.FailedNodes) != 1 || rep.FailedNodes[0] != "demux" {
		t.Fatalf("failed=%v", rep.FailedNodes)
	}
	if len(rep.BlockedNodes) != 2 {
		t.Fatalf("blocked=%v", rep.BlockedNodes)
	}
	// Both blocked nodes name the root failure, not their immediate parent.
	for _, id := range rep.BlockedNodes {
		if dep := d.States[id].Dep; dep != "demux" {
			t.Fatalf("%s dep=%q", id, dep)
		}
	}
	if rep.FirstBlockedDep != "demux" {
		t.Fatalf("first blocked dep=%q", rep.FirstBlockedDep)
	}
}

func TestRetryThenRecover(t *testing.T) {
	var calls int32
	d := pipeline.New(nil)
	_ = d.Add(pipeline.Node{ID: "flaky", MaxRetries: 3, Run: func(context.Context) (string, error) {
		if atomic.AddInt32(&calls, 1) < 3 {
			return "", errors.New("transient")
		}
		return "ok", nil
	}})
	rep := d.Execute(context.Background(), 1)
	if !rep.OK() || rep.RecoveredNodes != 1 {
		t.Fatalf("report=%+v", rep)
	}
	if got := d.States["flaky"].Attempts; got != 3 {
		t.Fatalf("attempts=%d", got)
	}
}

func TestRecoverableErrorCompletesNode(t *testing.T) {
	d := pipeline.New(nil)
	_ = d.Add(pipeline.Node{ID: "demux", Run: func(context.Context) (string, error) {
		return "partial", media.Recoverable("demux.parse", 10, media.ErrTruncated)
	}})
	rep := d.Execute(context.Background(), 1)
	if !rep.OK() || rep.RecoveredNodes != 1 {
		t.Fatalf("report=%+v", rep)
	}
	if got := d.States["demux"].Result; got != "partial" {
		t.Fatalf("result=%q", got)
	}
}

func TestIdempotencyKeyPreventsRewrite(t *testing.T) {
	var calls int32
	cp := pipeline.NewCheckpoint("")
	d := pipeline.New(cp)
	for i := 0; i < 2; i++ {
		d = pipeline.New(cp)
		_ = d.Add(pipeline.Node{ID: "hash", Key: "k1", Run: func(context.Context) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "h1", nil
		}})
		d.Execute(context.Background(), 1)
	}
	if calls != 1 {
		t.Fatalf("fn ran %d times", calls)
	}
}

func TestResumeOnlyRerunsIncompleteNodes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.json")
	var fastCalls, slowCalls int32
	build := func(fail bool) *pipeline.DAG {
		d := pipeline.New(mustLoad(t, path))
		_ = d.Add(pipeline.Node{ID: "fast", Key: "fast", Run: func(context.Context) (string, error) {
			atomic.AddInt32(&fastCalls, 1)
			return "f", nil
		}})
		_ = d.Add(pipeline.Node{ID: "slow", Key: "slow", Run: func(context.Context) (string, error) {
			atomic.AddInt32(&slowCalls, 1)
			if fail {
				return "", errors.New("boom")
			}
			return "s", nil
		}})
		return d
	}
	rep := build(true).Execute(context.Background(), 2)
	if rep.OK() {
		t.Fatal("expected failure")
	}
	if err := build(false).Execute(context.Background(), 2); err != nil {
		_ = err
	}
	if fastCalls != 1 {
		t.Fatalf("completed node re-ran: fast=%d", fastCalls)
	}
	if slowCalls != 2 {
		t.Fatalf("failed node did not re-run: slow=%d", slowCalls)
	}
}

func TestChangedKeyInvalidatesStoredState(t *testing.T) {
	cp := pipeline.NewCheckpoint("")
	d := pipeline.New(cp)
	var ran int32
	_ = d.Add(pipeline.Node{ID: "n", Key: "old", Run: func(context.Context) (string, error) {
		atomic.AddInt32(&ran, 1)
		return "x", nil
	}})
	d.Execute(context.Background(), 1)

	d2 := pipeline.New(cp)
	_ = d2.Add(pipeline.Node{ID: "n", Key: "new", Run: func(context.Context) (string, error) {
		atomic.AddInt32(&ran, 1)
		return "y", nil
	}})
	rep := d2.Execute(context.Background(), 1)
	if ran != 2 {
		t.Fatalf("node with a new key was incorrectly reused (ran=%d)", ran)
	}
	if rep.ResumedNodes != 0 {
		t.Fatalf("resumed=%d", rep.ResumedNodes)
	}
}

func TestCheckpointCorruptIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := pipeline.LoadCheckpoint(path)
	if !errors.Is(err, pipeline.ErrCheckpointCorrupt) {
		t.Fatalf("want ErrCheckpointCorrupt, got %v", err)
	}
}

func TestContextCancellationStops(t *testing.T) {
	d := pipeline.New(nil)
	_ = d.Add(pipeline.Node{ID: "hang", Run: func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rep := d.Execute(ctx, 1)
	if rep.OK() {
		t.Fatal("cancelled run reported success")
	}
}

func mustLoad(t *testing.T, path string) *pipeline.Checkpoint {
	t.Helper()
	cp, err := pipeline.LoadCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}
