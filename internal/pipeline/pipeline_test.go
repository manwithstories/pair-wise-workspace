package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"mediatime/internal/segment"
)

// slowWork 模拟带抖动的处理：每个任务睡眠不同时长以打乱完成顺序。
func slowWork(delay time.Duration) func(context.Context, Job) Result {
	return func(ctx context.Context, job Job) Result {
		select {
		case <-ctx.Done():
			return Result{Sequence: job.Sequence, Err: ErrCancelled}
		case <-time.After(delay):
			return Result{Sequence: job.Sequence, Fragment: &segment.ExtractedFragment{
				Sequence: job.Sequence, URI: job.URI,
			}}
		}
	}
}

func makeJobs(n int) []Job {
	jobs := make([]Job, n)
	for i := range jobs {
		jobs[i] = Job{Sequence: i, URI: fmt.Sprintf("seg%d.m4s", i)}
	}
	return jobs
}

func TestPoolRejectsWorkerLimit(t *testing.T) {
	if _, err := NewPool(Config{Workers: MaxWorkers + 1, QueueLen: 4}, slowWork(0)); !errors.Is(err, ErrWorkerLimit) {
		t.Fatalf("want ErrWorkerLimit, got %v", err)
	}
	if _, err := NewPool(Config{Workers: 0, QueueLen: 4}, slowWork(0)); !errors.Is(err, ErrWorkerLimit) {
		t.Fatalf("workers=0 must be rejected, got %v", err)
	}
}

func TestPoolProcessesAllAndMergeIsOrdered(t *testing.T) {
	pool, err := NewPool(Config{Workers: 4, QueueLen: 2}, slowWork(2*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	results, err := pool.Run(context.Background(), makeJobs(20))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	merged, err := Merge(results, 20)
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if merged.Completed != 20 || len(merged.Ordered) != 20 {
		t.Fatalf("completed = %d", merged.Completed)
	}
	for i, f := range merged.Ordered {
		if f.Sequence != i {
			t.Fatalf("merge order broken at %d: seq %d", i, f.Sequence)
		}
	}
}

func TestPoolCancellationDrainsInFlight(t *testing.T) {
	var started atomic.Int32
	work := func(ctx context.Context, job Job) Result {
		started.Add(1)
		select {
		case <-ctx.Done():
			return Result{Sequence: job.Sequence, Err: ErrCancelled}
		case <-time.After(50 * time.Millisecond):
			return Result{Sequence: job.Sequence, Fragment: &segment.ExtractedFragment{Sequence: job.Sequence}}
		}
	}
	pool, err := NewPool(Config{Workers: 2, QueueLen: 1}, work)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	results, err := pool.Run(ctx, makeJobs(16))
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	// 已提交的结果必须被排空（results 非空），且不会丢失在途任务的结论。
	if len(results) == 0 {
		t.Fatal("cancelled pool must drain committed results")
	}
	if started.Load() == 0 {
		t.Fatal("expected at least one task to start before cancellation")
	}
	// 归并器在取消场景下仍可确定性整理已有结果。
	if _, err := Merge(results, 16); err != nil {
		t.Fatalf("merge after cancel failed: %v", err)
	}
}

func TestPoolQueueFullBackpressure(t *testing.T) {
	// 队列长度 1、单 worker 长任务：投递协程在队列满时阻塞，
	// 取消必须解除阻塞且不泄漏 goroutine。
	work := func(ctx context.Context, job Job) Result {
		select {
		case <-ctx.Done():
			return Result{Sequence: job.Sequence, Err: ErrCancelled}
		case <-time.After(100 * time.Millisecond):
			return Result{Sequence: job.Sequence}
		}
	}
	pool, err := NewPool(Config{Workers: 1, QueueLen: 1}, work)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	done := make(chan struct{})
	go func() {
		_, _ = pool.Run(ctx, makeJobs(32))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not return after cancellation; likely goroutine leak")
	}
}

func TestMergeRecordsFailuresAndMissing(t *testing.T) {
	boomed := errors.New("boom")
	results := []Result{
		{Sequence: 3, Fragment: &segment.ExtractedFragment{Sequence: 3}},
		{Sequence: 0, Fragment: &segment.ExtractedFragment{Sequence: 0}},
		{Sequence: 2, Err: boomed},
		{Sequence: 0, Fragment: &segment.ExtractedFragment{Sequence: 0}}, // 重复成功结果
	}
	merged, err := Merge(results, 5)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Completed != 2 || merged.Ordered[0].Sequence != 0 || merged.Ordered[1].Sequence != 3 {
		t.Fatalf("ordered merge wrong: %+v", merged.Ordered)
	}
	if !errors.Is(merged.Failed[2], boomed) {
		t.Fatal("failure not retained with sequence")
	}
	if len(merged.Missing) != 2 || merged.Missing[0] != 1 || merged.Missing[1] != 4 {
		t.Fatalf("missing detection wrong: %v", merged.Missing)
	}
}

func TestTrySubmitQueueFullAndCancel(t *testing.T) {
	block := make(chan struct{})
	var running atomic.Int32
	work := func(ctx context.Context, job Job) Result {
		running.Add(1)
		defer running.Add(-1)
		select {
		case <-ctx.Done():
			return Result{Sequence: job.Sequence, Err: ErrCancelled}
		case <-block:
			return Result{Sequence: job.Sequence, Fragment: &segment.ExtractedFragment{Sequence: job.Sequence}}
		}
	}
	pool, err := NewPool(Config{Workers: 1, QueueLen: 1}, work)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results, wait := pool.startWorkers(ctx)

	// 第一个任务被 worker 取走后，第二个填满唯一队列槽位，第三次必须报队列满。
	if err := pool.TrySubmit(ctx, Job{Sequence: 0}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for running.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if running.Load() == 0 {
		t.Fatal("worker never picked up the first job")
	}
	if err := pool.TrySubmit(ctx, Job{Sequence: 1}); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if err := pool.TrySubmit(ctx, Job{Sequence: 2}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("want ErrQueueFull, got %v", err)
	}

	// 取消后 TrySubmit 立即返回取消错误而不是阻塞。
	cancel()
	if err := pool.TrySubmit(ctx, Job{Sequence: 3}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled after cancel, got %v", err)
	}
	close(block)
	pool.CloseSubmit()
	collected, err := Collect(context.Background(), results)
	wait()
	if len(collected) == 0 {
		t.Fatal("committed results must be drained")
	}
	_ = err
}

func TestMergeRejectsOutOfRangeAndConflict(t *testing.T) {
	if _, err := Merge([]Result{{Sequence: 9}}, 3); err == nil {
		t.Fatal("out-of-range sequence must be rejected")
	}
	conflict := []Result{
		{Sequence: 1, Fragment: &segment.ExtractedFragment{Sequence: 1, URI: "a"}},
		{Sequence: 1, Fragment: &segment.ExtractedFragment{Sequence: 1, URI: "b"}},
	}
	if _, err := Merge(conflict, 3); err == nil {
		t.Fatal("conflicting same-sequence results must be rejected")
	}
}

func TestMergeDeterministicAcrossOrderings(t *testing.T) {
	base := makeJobs(8)
	run := func() *Merged {
		results := make([]Result, len(base))
		for i, j := range base {
			// 故意逆序放入，模拟最坏完成顺序。
			results[len(base)-1-i] = Result{
				Sequence: j.Sequence,
				Fragment: &segment.ExtractedFragment{Sequence: j.Sequence, URI: j.URI},
			}
		}
		m, err := Merge(results, len(base))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a, b := run(), run()
	if a.Completed != b.Completed || len(a.Ordered) != len(b.Ordered) {
		t.Fatal("non-deterministic completion counts")
	}
	for i := range a.Ordered {
		if a.Ordered[i].URI != b.Ordered[i].URI || a.Ordered[i].Sequence != b.Ordered[i].Sequence {
			t.Fatal("merge output differs across runs with identical inputs")
		}
	}
}
