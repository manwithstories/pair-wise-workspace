package runner

import (
	"context"
	"testing"
	"time"

	"media-audit/internal/checkpoint"
	"media-audit/internal/stream"
)

func makeJobs(n int) []Job {
	jobs := make([]Job, n)
	for i := range jobs {
		// 极简 H.264 流：起始码 + IDR 头 + 净荷。
		data := []byte{0, 0, 0, 1, 0x65, 'i', 'd', 'r'}
		jobs[i] = Job{
			JobID:       "job-" + string(rune('A'+i)),
			TrackID:     i,
			Codec:       stream.H264,
			BlockFrames: 10,
			Data:        data,
		}
	}
	return jobs
}

func TestRunJobsConcurrentAndSuccess(t *testing.T) {
	res := RunJobs(context.Background(), makeJobs(4), Config{Concurrency: 2})
	if len(res) != 4 {
		t.Fatalf("结果数=%d, 期望 4", len(res))
	}
	for id, r := range res {
		if r.Err != nil {
			t.Fatalf("任务 %s 失败: %v", id, r.Err)
		}
		if len(r.Scan.AUs) != 1 {
			t.Fatalf("任务 %s AU 数异常", id)
		}
	}
}

func TestDuplicateJobIDsExecutedOnce(t *testing.T) {
	jobs := append(makeJobs(2), makeJobs(1)...)
	jobs[2].JobID = jobs[0].JobID // 重复 ID
	res := RunJobs(context.Background(), jobs, Config{Concurrency: 2})
	if len(res) != 2 {
		t.Fatalf("去重后任务数=%d, 期望 2", len(res))
	}
}

func TestCancellationPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消
	res := RunJobs(ctx, makeJobs(3), Config{Concurrency: 1})
	cancelled := 0
	for _, r := range res {
		if r.Err != nil {
			cancelled++
		}
	}
	if cancelled == 0 {
		t.Fatalf("取消后应有任务收到错误")
	}
}

func TestPreflightRejectsOversizeTask(t *testing.T) {
	big := makeJobs(1)
	big[0].Data = make([]byte, maxTaskBytes+1)
	if err := Preflight(big); err == nil {
		t.Fatalf("超限任务必须被预检拦截")
	}
}

func TestCheckpointResumeReusesBlocks(t *testing.T) {
	dir := t.TempDir()
	jobs := makeJobs(1)
	ref := checkpoint.InputRef{Size: int64(len(jobs[0].Data)), SHA256: hashData(jobs[0].Data)}
	states := []checkpoint.TrackState{{
		TrackID: 0, Codec: "h264", InputSize: ref.Size, InputSHA256: ref.SHA256,
		Completed: []stream.BlockRecord{{Number: 0, Start: 0, End: ref.Size, SHA256: "x"}},
	}}
	if err := PersistCheckpoint(dir, "batch1", "out", states); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareResume(dir, "batch1", map[int]checkpoint.InputRef{0: ref})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Outcome != checkpoint.OutcomeReusable || plan.ReusedCount != 1 {
		t.Fatalf("恢复计划错误: %+v", plan)
	}
}

func TestCheckpointResumeResetsOnChangedInput(t *testing.T) {
	dir := t.TempDir()
	old := checkpoint.InputRef{Size: 8, SHA256: "old"}
	states := []checkpoint.TrackState{{
		TrackID: 0, Codec: "h264", InputSize: old.Size, InputSHA256: old.SHA256,
		Completed: []stream.BlockRecord{{Number: 0, SHA256: "x"}},
	}}
	_ = PersistCheckpoint(dir, "batch2", "out", states)
	current := map[int]checkpoint.InputRef{0: {Size: 9, SHA256: "new"}}
	plan, _ := PrepareResume(dir, "batch2", current)
	if plan.Outcome != checkpoint.OutcomePartial {
		t.Fatalf("输入变化应 partial, got %s", plan.Outcome)
	}
	if plan.ReusedCount != 0 {
		t.Fatalf("变化输入不应复用任何块")
	}
}

func TestRunWithCancelReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_ = ctx
	done := make(chan struct{})
	go func() {
		RunWithCancel(makeJobs(2), Config{Concurrency: 1}, cancel)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("取消编排未能在时限内返回")
	}
}
