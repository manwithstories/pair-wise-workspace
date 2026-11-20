// Package runner 编排多节目并发审计：限制并发与打开文件数，传播取消，
// 按块提交检查点，并在中断后仅复用校验通过的块恢复批次。
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"

	"media-audit/internal/checkpoint"
	"media-audit/internal/stream"
)

// MaxOpenFiles 限制单个批处理同时打开的文件数。
const MaxOpenFiles = 16

// Job 是一条表示层的扫描任务。
type Job struct {
	JobID       string
	TrackID     int
	Codec       stream.Codec
	BlockFrames int
	Data        []byte
}

// JobResult 是单条表示层的处理结果。
type JobResult struct {
	JobID    string
	TrackID  int
	Scan     *stream.ScanResult
	InputRef checkpoint.InputRef
	Blocks   []stream.BlockRecord
	Err      error
}

// Config 控制批处理行为。
type Config struct {
	Concurrency int
	CheckpointDir string
}

// hashData 计算输入字节的 SHA-256 十六进制摘要。
func hashData(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RunJobs 并发执行扫描任务，受 ctx 取消控制，返回按 JobID 去重后的结果。
// 重复提交的同一 JobID 只执行一次，结果共享。
func RunJobs(ctx context.Context, jobs []Job, cfg Config) map[string]*JobResult {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	sem := make(chan struct{}, cfg.Concurrency)
	fileSem := make(chan struct{}, MaxOpenFiles)

	var mu sync.Mutex
	results := map[string]*JobResult{}
	var wg sync.WaitGroup

	for _, job := range jobs {
		// 去重：同一 JobID 已登记则跳过。
		mu.Lock()
		if _, exists := results[job.JobID]; exists {
			mu.Unlock()
			continue
		}
		results[job.JobID] = nil // 占位，阻止重复任务并发进入
		mu.Unlock()

		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				results[j.JobID] = &JobResult{JobID: j.JobID, TrackID: j.TrackID, Err: ctx.Err()}
				mu.Unlock()
				return
			}
			defer func() { <-sem }()

			fileSem <- struct{}{}
			res := scanOne(ctx, j)
			<-fileSem

			mu.Lock()
			results[j.JobID] = res
			mu.Unlock()
		}(job)
	}
	wg.Wait()
	return results
}

// scanOne 执行单任务扫描，尊重取消。
func scanOne(ctx context.Context, j Job) *JobResult {
	if err := ctx.Err(); err != nil {
		return &JobResult{JobID: j.JobID, TrackID: j.TrackID, Err: err}
	}
	scanner := stream.NewScanner(j.Codec, j.TrackID, j.BlockFrames)
	scan, err := scanner.Scan(newBytesReader(j.Data))
	if err != nil {
		return &JobResult{JobID: j.JobID, TrackID: j.TrackID, Err: err}
	}
	ref := checkpoint.InputRef{Size: int64(len(j.Data)), SHA256: hashData(j.Data)}
	return &JobResult{
		JobID:    j.JobID,
		TrackID:  j.TrackID,
		Scan:     scan,
		InputRef: ref,
		Blocks:   scan.Blocks,
	}
}

// ErrCancelled 在批处理被显式取消时返回。
var ErrCancelled = errors.New("批处理已取消")
