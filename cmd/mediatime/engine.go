// Package main 是 mediatime 验收适配入口：在纯本地、无外部服务的条件下
// 串起清单解析、有界并发提取、时钟修复、随机访问索引与指标报告。
package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"mediatime/internal/clock"
	"mediatime/internal/fixture"
	"mediatime/internal/manifest"
	"mediatime/internal/pipeline"
	"mediatime/internal/report"
	"mediatime/internal/segment"
)

// runConfig 是一次 demo 运行的全部显式参数。
type runConfig struct {
	seed          int64
	workers       int
	queue         int
	fault         fixture.Fault
	cancelAfter   int // >0 时处理该数量片段后取消
	thresholds    report.Thresholds
}

// engine 聚合各子系统，避免把编排逻辑塞进 main。
type engine struct {
	cfg runConfig
}

// submitWithBackpressure 在有界队列上投递任务：队列满时短暂退避重试，
// 形成确定性背压而不丢弃任务；ctx 取消则立即返回 ErrCancelled。
func submitWithBackpressure(pool *pipeline.Pool, ctx context.Context, job pipeline.Job) error {
	for {
		err := pool.TrySubmit(ctx, job)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pipeline.ErrQueueFull) {
			return err
		}
		select {
		case <-ctx.Done():
			return pipeline.ErrCancelled
		case <-time.After(time.Microsecond * 100):
		}
	}
}

// runResult 携带报告与终态，供 CLI 决定退出码。
type runResult struct {
	metrics   *report.Metrics
	exitCode  int
}

// run 执行完整管线并返回确定性报告。
func (e *engine) run(ctx context.Context) (*runResult, error) {
	gen := fixture.NewGenerator(e.cfg.seed)
	scenario, err := gen.Generate(fixture.DefaultFragmentCount, e.cfg.fault)
	if err != nil {
		return nil, err
	}

	// 冲突序号清单：解析阶段即拒绝，退出码 2。
	playlist := scenario.HLSPlaylist
	if e.cfg.fault == fixture.FaultConflict {
		playlist = scenario.ConflictPlaylist
	}
	mf, err := manifest.ParseHLS([]byte(playlist))
	if err != nil {
		return e.invalidResult(scenario, err), nil
	}

	// 建立片段规格索引，缺片序号在其中缺失。
	specBySeq := map[int]fixture.FragmentSpec{}
	for _, sp := range scenario.FragmentSpec {
		specBySeq[sp.Sequence] = sp
	}

	// 取消控制。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var submitted int
	// maxAttempted 记录 worker 已开始处理的最大片段序号，用于在
	// cancel-after 个片段（含故障片）被尝试后触发取消。
	var maxAttempted atomic.Int32
	// attemptSig 在每次 worker 接走任务后非阻塞发信号，驱动取消判断，
	// 避免任何轮询延迟导致过度处理。
	attemptSig := make(chan struct{}, 1)

	work := func(c context.Context, job pipeline.Job) pipeline.Result {
		// 单片 64MiB 有界读取（此处数据来自内存夹具，仍走统一入口）。
		for {
			cur := maxAttempted.Load()
			if int32(job.Sequence) <= cur || maxAttempted.CompareAndSwap(cur, int32(job.Sequence)) {
				break
			}
		}
		select {
		case attemptSig <- struct{}{}:
		default:
		}
		data := job.Data
		if len(data) > segment.MaxSegmentBytes {
			return pipeline.Result{Sequence: job.Sequence, Err: segment.ErrSegmentTooLarge}
		}
		frag, err := segment.ExtractFragment(job.Sequence, job.URI, data)
		if err != nil {
			return pipeline.Result{Sequence: job.Sequence, Err: err}
		}
		return pipeline.Result{Sequence: job.Sequence, Fragment: frag}
	}
	pool, err := pipeline.NewPool(pipeline.Config{
		Workers: e.cfg.workers, QueueLen: e.cfg.queue,
	}, work)
	if err != nil {
		return nil, err
	}

	// 用 Start/TrySubmit 生命周期，精确演示"取消时队列满"背压。
	resultsCh, wait := pool.StartWorkers(runCtx)
	stopWatch := make(chan struct{})
	if e.cfg.cancelAfter > 0 {
		go func() {
			for {
				select {
				case <-stopWatch:
					return
				case <-runCtx.Done():
					return
				case <-attemptSig:
					// 第 cancelAfter+1 片（含故障片）已被 worker 接走即取消。
					if int(maxAttempted.Load()) >= e.cfg.cancelAfter {
						cancel()
						return
					}
				}
			}
		}()
	}
	for seq := 0; seq < len(mf.Fragments); seq++ {
		sp, ok := specBySeq[seq]
		if !ok {
			continue // 缺片：不投递，归并器会识别为 missing
		}
		job := pipeline.Job{
			Sequence: seq, URI: sp.URI, Data: fixture.BuildFragmentBytes(sp),
		}
		// 有界背压：队列满时不丢任务，阻塞等待 worker 腾出槽位，
		// 但随时响应取消；queue-full 故障借此确定性地反复走背压路径。
		if err := submitWithBackpressure(pool, runCtx, job); err != nil {
			if errors.Is(err, pipeline.ErrCancelled) {
				break
			}
			return nil, err
		}
		submitted++
	}
	close(stopWatch)
	pool.CloseSubmit()
	results, poolErr := pipeline.Collect(runCtx, resultsCh)
	wait()

	merged, mergeErr := pipeline.Merge(results, len(mf.Fragments))
	if mergeErr != nil {
		return nil, mergeErr
	}

	// 装配时钟修复输入与期望时间轴。
	var allSamples []clock.Sample
	var mediaBytes int64
	var frags []*segment.ExtractedFragment
	for _, f := range merged.Ordered {
		frags = append(frags, f)
		allSamples = append(allSamples, f.ToClockSamples()...)
		for _, s := range f.Samples {
			mediaBytes += int64(s.Size)
		}
	}
	expected := map[int]clock.Tick{}
	for _, fr := range mf.Fragments {
		expected[fr.Sequence] = clock.Tick(fr.StartTicks)
	}

	var tl *clock.Timeline
	var idx *segment.Index
	corrupted := countCorrupted(merged, results)
	if len(allSamples) > 0 {
		tl, err = clock.RepairSamples(allSamples, clock.RepairConfig{
			Window:      8,
			OutlierTick: clock.SecondsToTick(0.05),
			Expected:    expected,
		}, merged.Missing)
		if err != nil {
			return nil, fmt.Errorf("clock repair: %w", err)
		}
		idx, err = segment.BuildIndex(frags, tl)
		if err != nil {
			// 索引失败属于片段损坏，仍产出部分报告。
			corrupted++
		}
	}

	cancelled := errors.Is(poolErr, pipeline.ErrCancelled) || runCtx.Err() != nil
	errorCategory := classifyError(poolErr, merged, cancelled)

	metrics := report.Compute(report.Inputs{
		ManifestFragments: len(mf.Fragments),
		TargetBPS:         scenario.TargetBPS,
		MediaBytes:        mediaBytes,
		Timeline:          tl,
		Index:             idx,
		Corrupted:         corrupted,
		Cancelled:         cancelled,
		ErrorCategory:     errorCategory,
		ManifestStarts:    expected,
		Thresholds:        e.cfg.thresholds,
		CompletedOverride: merged.Completed,
	})
	return &runResult{metrics: metrics, exitCode: metrics.ExitCode()}, nil
}

func (e *engine) invalidResult(sc *fixture.Scenario, cause error) *runResult {
	m := report.Compute(report.Inputs{
		ManifestFragments: sc.Fragments,
		TargetBPS:         sc.TargetBPS,
		ErrorCategory:     manifestCategory(cause),
		Thresholds:        e.cfg.thresholds,
	})
	return &runResult{metrics: m, exitCode: m.ExitCode()}
}

// manifestCategory 把清单解析错误映射为短类别名。
func manifestCategory(err error) string {
	switch {
	case errors.Is(err, manifest.ErrDuplicateSequence):
		return "duplicate_sequence"
	case errors.Is(err, manifest.ErrPathOutOfScope):
		return "path_out_of_scope"
	case errors.Is(err, manifest.ErrManifestTooLarge), errors.Is(err, manifest.ErrTooManyFragments):
		return "manifest_limit"
	default:
		return "invalid_manifest"
	}
}

func countCorrupted(merged *pipeline.Merged, results []pipeline.Result) int {
	// 损坏 = 有失败结果的片段数（去重）。
	bad := map[int]bool{}
	for _, r := range results {
		if r.Err != nil {
			bad[r.Sequence] = true
		}
	}
	return len(bad)
}

func classifyError(poolErr error, merged *pipeline.Merged, cancelled bool) string {
	// 片段损坏类别优先：即使同时取消，也要保留可定位的 truncated_box 等根因。
	for seq, err := range merged.Failed {
		_ = seq
		return errorName(err)
	}
	if cancelled {
		return "cancelled"
	}
	if poolErr != nil {
		return errorName(poolErr)
	}
	return ""
}

// errorName 提取可定位的纯错误类别字符串（去掉包前缀与片段定位包装）。
func errorName(err error) string {
	var fe *segment.FragmentError
	if errors.As(err, &fe) {
		return categoryOf(fe.Err)
	}
	return categoryOf(err)
}

// categoryOf 把包级哨兵错误转换为验收规定的短类别名。
func categoryOf(err error) string {
	switch {
	case errors.Is(err, segment.ErrTruncatedBox):
		return "truncated_box"
	case errors.Is(err, segment.ErrBoxOverflow):
		return "truncated_box"
	case errors.Is(err, segment.ErrNoMoof), errors.Is(err, segment.ErrNoMdat):
		return "truncated_box"
	case errors.Is(err, segment.ErrSegmentTooLarge):
		return "segment_too_large"
	case errors.Is(err, segment.ErrEmptySegment):
		return "empty_segment"
	case errors.Is(err, pipeline.ErrCancelled):
		return "cancelled"
	case errors.Is(err, pipeline.ErrQueueFull):
		return "queue_full"
	default:
		return err.Error()
	}
}
