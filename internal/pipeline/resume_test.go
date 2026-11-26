package pipeline_test

import (
	"context"
	"fmt"
	"testing"

	"mediascan/internal/pipeline"
	"mediascan/internal/store"
)

func testTask(t *testing.T, name, hash string, params store.Params) store.Task {
	t.Helper()
	jobID := store.JobID("/media/" + name)
	task, err := store.NewTask(jobID, name, hash, params)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func testJobs(t *testing.T, n int, params store.Params) []pipeline.Job {
	t.Helper()
	jobs := make([]pipeline.Job, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("file-%d.mp4", i)
		task := testTask(t, name, fmt.Sprintf("hash-%d", i), params)
		jobs = append(jobs, pipeline.Job{Index: i, Path: "/media/" + name, Task: task})
	}
	return jobs
}

func fakeAnalyze(calls *int) func(context.Context, pipeline.Job) (*pipeline.FileResult, error) {
	return func(_ context.Context, job pipeline.Job) (*pipeline.FileResult, error) {
		*calls++
		return &pipeline.FileResult{
			Task:   job.Task,
			Index:  job.Index,
			Path:   job.Path,
			Name:   job.Task.AssetName,
			SHA256: job.Task.AssetSHA256,
		}, nil
	}
}

func TestCheckpointResumeSkipsCommittedTasks(t *testing.T) {
	params := store.Params{Workers: 1, Seed: 17}
	jobs := testJobs(t, 5, params)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	committed := 0
	calls := 0
	pipeline.Run(ctx, jobs, pipeline.Options{
		Workers: 1,
		Store:   st,
		Analyze: fakeAnalyze(&calls),
		OnCommitted: func(string) {
			committed++
			if committed == 2 {
				cancel()
			}
		},
	})
	cancel()
	if committed != 2 || calls != 2 {
		t.Fatalf("interrupted run committed=%d calls=%d, want 2/2", committed, calls)
	}

	for i := 0; i < 2; i++ {
		cp, ok, err := st.LoadCommitted(jobs[i].Task)
		if err != nil || !ok || cp.Status != store.StatusCommitted {
			t.Fatalf("checkpoint %d not committed: ok=%v err=%v", i, ok, err)
		}
	}

	calls = 0
	outcomes := pipeline.Run(context.Background(), jobs, pipeline.Options{
		Workers: 4,
		Store:   st,
		Analyze: fakeAnalyze(&calls),
	})
	skipped := 0
	for _, o := range outcomes {
		if o.Skipped {
			skipped++
		}
		if o.Err != nil {
			t.Fatal(o.Err)
		}
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
	if calls != 3 {
		t.Fatalf("new analysis calls = %d, want 3", calls)
	}

	rep, err := pipeline.OrderedMerge(context.Background(), outcomes, pipeline.MergeConfig{Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Metrics.Files != 5 || rep.Metrics.ResumeSkipped != 2 {
		t.Fatalf("metrics files=%d skipped=%d", rep.Metrics.Files, rep.Metrics.ResumeSkipped)
	}
}

func TestDuplicateTasksMergeOnce(t *testing.T) {
	params := store.Params{Workers: 4, Seed: 1}
	jobs := testJobs(t, 2, params)
	retry := jobs[0]
	outcomes := pipeline.Run(context.Background(), []pipeline.Job{jobs[0], retry, jobs[1]}, pipeline.Options{
		Workers: 4,
		Analyze: fakeAnalyze(new(int)),
	})
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2 after task-key deduplication", len(outcomes))
	}
	rep, err := pipeline.OrderedMerge(context.Background(), outcomes, pipeline.MergeConfig{Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Metrics.Files != 2 || len(rep.Inputs) != 2 {
		t.Fatalf("files=%d inputs=%d, want 2", rep.Metrics.Files, len(rep.Inputs))
	}
}

func TestChangedAssetRejectsOldCheckpoint(t *testing.T) {
	params := store.Params{Workers: 2, Seed: 3}
	jobID := store.JobID("/media/change.mp4")
	oldTask, err := store.NewTask(jobID, "change.mp4", "old-hash", params)
	if err != nil {
		t.Fatal(err)
	}
	newTask, err := store.NewTask(jobID, "change.mp4", "new-hash", params)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveCommitted(oldTask, &pipeline.FileResult{Task: oldTask}); err != nil {
		t.Fatal(err)
	}

	_, _, err = st.Resolve(newTask)
	if !store.IsMismatch(err) {
		t.Fatalf("Resolve error = %v, want fingerprint mismatch", err)
	}

	calls := 0
	outcomes := pipeline.Run(context.Background(), []pipeline.Job{{Index: 0, Task: newTask}}, pipeline.Options{
		Workers: 1,
		Store:   st,
		Analyze: fakeAnalyze(&calls),
	})
	if calls != 0 {
		t.Fatalf("analyze called %d times after mismatch", calls)
	}
	if len(outcomes) != 1 || !outcomes[0].FingerprintMismatch || outcomes[0].Err == nil {
		t.Fatalf("outcomes = %+v, want one fingerprint mismatch", outcomes)
	}
}
