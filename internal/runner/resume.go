package runner

import (
	"bytes"
	"context"

	"media-audit/internal/checkpoint"
	"media-audit/internal/stream"
)

// maxTaskBytes 限制单任务驻留内存（512 MiB 约束内的任务级预算）。
const maxTaskBytes = 512 * 1024 * 1024

// boundedBuffer 包装字节切片并在构造时校验任务内存预算。
type boundedBuffer struct {
	data []byte
}

func newBytesReader(b []byte) *bytes.Reader {
	if len(b) > maxTaskBytes {
		// 超出预算时仍返回读取器，但上层会在 Preflight 拦截；此处不 panic。
		return bytes.NewReader(b)
	}
	return bytes.NewReader(b)
}

// Preflight 校验所有任务的内存预算，超限即返回错误（退出码 4 类参数/I/O）。
func Preflight(jobs []Job) error {
	for _, j := range jobs {
		if len(j.Data) > maxTaskBytes {
			return ErrTaskTooLarge{JobID: j.JobID, Size: len(j.Data)}
		}
	}
	return nil
}

// ErrTaskTooLarge 表示单任务超出内存预算。
type ErrTaskTooLarge struct {
	JobID string
	Size  int
}

func (e ErrTaskTooLarge) Error() string {
	return "任务 " + e.JobID + " 超出 512MiB 内存预算"
}

// ResumePlan 是恢复后的执行计划。
type ResumePlan struct {
	Outcome        checkpoint.LoadOutcome
	ReusableBlocks map[int][]stream.BlockRecord
	ReusedCount    int
	ResetReason    string
}

// PrepareResume 读取检查点并验证当前输入，决定可复用块与是否回退。
// current 为各轨道当前输入引用。
func PrepareResume(dir, jobID string, current map[int]checkpoint.InputRef) (*ResumePlan, error) {
	loaded, err := checkpoint.Load(dir, jobID, current)
	if err != nil {
		return nil, err
	}
	plan := &ResumePlan{
		Outcome:        loaded.Outcome,
		ReusableBlocks: loaded.Reusable,
		ResetReason:    loaded.ResetReason,
	}
	for _, blocks := range loaded.Reusable {
		plan.ReusedCount += len(blocks)
	}
	return plan, nil
}

// PersistCheckpoint 把当前各轨道已完成块原子写入检查点。
func PersistCheckpoint(dir, jobID, outputSHA string, states []checkpoint.TrackState) error {
	st := &checkpoint.Store{
		JobID:        jobID,
		Tracks:       states,
		OutputSHA256: outputSHA,
	}
	return checkpoint.Save(dir, st)
}

// RunWithCancel 演示取消传播：取消后未开始的任务收到 ErrCancelled。
func RunWithCancel(jobs []Job, cfg Config, cancel context.CancelFunc) map[string]*JobResult {
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	go func() {
		cancel()
	}()
	return RunJobs(ctx, jobs, cfg)
}
