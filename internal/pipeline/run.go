// Package pipeline 协调 ingest、analyze、graph、checkpoint 与 repack
// 各模块，提供可中断、可续跑且重复执行产物一致的媒体核验流水线。
package pipeline

import (
	"errors"
	"fmt"
	"time"

	"bcastqc/internal/analyze"
	"bcastqc/internal/checkpoint"
	"bcastqc/internal/model"
	"bcastqc/internal/repack"
)

// Config 描述一次流水线运行的输入参数。
type Config struct {
	JobID       string
	OutDir      string
	Seed        int64
	Tracks      int
	Samples     int64
	Bandwidth   []int64
	Faults      FaultMask
	// InterruptAfter 为 >0 时，在完成指定阶段步数后抛出受控中断。
	InterruptAfter int
	// ReorderWindow 为包乱序窗口大小。
	ReorderWindow int64
	// DriftTolerance 为跨轨漂移容差（采样点）。
	DriftTolerance int64
	// NoAnchors 为 true 时夹具不放置任何同步锚点，用于演示缺标 N/A。
	NoAnchors bool
}

// FaultMask 控制夹具生成时注入的故障类型。
type FaultMask struct {
	Duplicate   bool
	Reorder     bool
	BurstLoss   bool
	CRCError    bool
	Truncation  bool
	RiffOverflow bool
	Cycle       bool
}

// AllFaults 返回启用全部故障类型的掩码。
func AllFaults() FaultMask {
	return FaultMask{
		Duplicate: true, Reorder: true, BurstLoss: true, CRCError: true,
		Truncation: true, RiffOverflow: true, Cycle: true,
	}
}

// Report 是一次流水线运行（或续跑）的终端汇报数据。
type Report struct {
	Case               string       `json:"case"`
	Tracks             int          `json:"tracks"`
	DamageSegments     int          `json:"damage_segments"`
	LocErrorSamples    int64        `json:"loc_error_samples"`
	ResumeMs           int64        `json:"resume_ms"`
	RecoveredSamples   int64        `json:"recovered_samples"`
	RerunSHAMatch      bool         `json:"rerun_sha_match"`
	SHA256             string       `json:"sha256"`
	AnchorNA           bool         `json:"anchor_na"`
	StructureReports   []analyze.StructureReport `json:"-"`
	DamageReports      []*analyze.DamageReport   `json:"-"`
	Manifest           *repack.Manifest          `json:"-"`
}

// Runner 持有单次任务的运行态与检查点。
type Runner struct {
	cfg   Config
	store *checkpoint.Store
}

// NewRunner 构造流水线运行器。
func NewRunner(cfg Config) *Runner {
	return &Runner{cfg: cfg, store: checkpoint.NewStore(cfg.OutDir)}
}

// Run 执行流水线；若存在历史检查点则自动续跑，保证采样总数与校验和稳定。
func (r *Runner) Run() (*Report, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	start := time.Now()

	st, existed, err := r.store.Load()
	if err != nil {
		return nil, err
	}
	if existed && st.JobID != r.cfg.JobID {
		return nil, fmt.Errorf("%w: 检查点任务 %q 与当前 %q 不一致",
			model.ErrUnrecoverable, st.JobID, r.cfg.JobID)
	}
	if !existed {
		st = &checkpoint.State{
			JobID:        r.cfg.JobID,
			Steps:        map[string]int64{},
			TrackSamples: map[int]int64{},
		}
	}

	// 阶段 1：生成夹具并完成 ingest（WAV 核验 + 包序整理）。
	fixture, err := BuildFixture(r.cfg)
	if err != nil {
		return nil, err
	}
	if err := checkpoint.VerifySampleCounts(st, fixture.TrackSampleCounts()); err != nil {
		return nil, err
	}
	if !existed {
		st.TrackSamples = fixture.TrackSampleCounts()
	}
	if err := r.advance(st, checkpoint.StageIngest, 1); err != nil {
		return nil, err
	}
	if r.shouldInterrupt(st, 1) {
		return nil, r.interrupt(st, start)
	}

	// 阶段 2：结构分析 + 损坏定位。
	structReps, damageReps, timelines, err := r.runAnalyze(fixture)
	if err != nil {
		return nil, err
	}
	if err := r.advance(st, checkpoint.StageAnalyze, int64(len(fixture.Tracks))); err != nil {
		return nil, err
	}
	if r.shouldInterrupt(st, 2) {
		return nil, r.interrupt(st, start)
	}

	// 阶段 3：依赖图拓扑、漂移与越界核验。
	if err := r.runGraph(fixture, timelines); err != nil {
		return nil, err
	}
	if err := r.advance(st, checkpoint.StageGraph, 1); err != nil {
		return nil, err
	}

	// 阶段 4：再封装母版、清单与 SHA-256。
	manifest, err := r.runRepack(fixture, timelines)
	if err != nil {
		return nil, err
	}
	// 在覆盖检查点前捕获上一轮完整产物的 SHA，用于重复执行一致性核验。
	previousSHA := st.OutputSHA
	st.RecoveredSamples = manifest.RecoveredSamples
	st.OutputSHA = manifest.SHA256
	st.RunCount++
	st.Stage = checkpoint.StageComplete
	if err := r.store.Save(st); err != nil {
		return nil, err
	}

	// 重复执行一致性：续跑/重跑时新一轮 SHA 必须与上一轮完全相同。
	rerunMatch := true
	if previousSHA != "" {
		rerunMatch = manifest.SHA256 == previousSHA
		if !rerunMatch {
			return nil, fmt.Errorf("%w: 重复执行 SHA-256 不一致 旧=%s 新=%s",
				model.ErrUnrecoverable, previousSHA, manifest.SHA256)
		}
	}

	damageCount := 0
	var maxLocErr int64
	anchorNA := true
	for _, dr := range damageReps {
		damageCount += len(dr.Segments)
		if dr.MaxLocError > maxLocErr {
			maxLocErr = dr.MaxLocError
		}
	}
	for _, tl := range timelines {
		if len(tl.Anchors) >= 2 {
			anchorNA = false
		}
	}

	return &Report{
		Case:             r.cfg.JobID,
		Tracks:           len(fixture.Tracks),
		DamageSegments:   damageCount,
		LocErrorSamples:  maxLocErr,
		ResumeMs:         time.Since(start).Milliseconds(),
		RecoveredSamples: manifest.RecoveredSamples,
		RerunSHAMatch:    rerunMatch,
		SHA256:           manifest.SHA256,
		AnchorNA:         anchorNA,
		StructureReports: structReps,
		DamageReports:    damageReps,
		Manifest:         manifest,
	}, nil
}

// validate 校验运行参数，非法参数映射退出码 2。
func (r *Runner) validate() error {
	if r.cfg.Tracks <= 0 || r.cfg.Tracks > model.MaxTracks {
		return fmt.Errorf("%w: tracks 必须在 [1,%d]，得到 %d",
			model.ErrInvalidInput, model.MaxTracks, r.cfg.Tracks)
	}
	if r.cfg.Samples <= 0 {
		return fmt.Errorf("%w: samples 必须为正，得到 %d", model.ErrInvalidInput, r.cfg.Samples)
	}
	if err := model.CheckSampleCount(r.cfg.Samples); err != nil {
		return err
	}
	if len(r.cfg.Bandwidth) == 0 {
		return fmt.Errorf("%w: bandwidth 至少需要一个档位", model.ErrInvalidInput)
	}
	if r.cfg.OutDir == "" {
		return fmt.Errorf("%w: out 目录不能为空", model.ErrInvalidInput)
	}
	if r.cfg.ReorderWindow < 1 {
		r.cfg.ReorderWindow = 4
	}
	return nil
}

// advance 记录阶段进度并持久化检查点。
func (r *Runner) advance(st *checkpoint.State, stage checkpoint.Stage, steps int64) error {
	st.Stage = stage
	st.Steps[string(stage)] = steps
	return r.store.Save(st)
}

// shouldInterrupt 判断当前步数是否达到受控中断点。
func (r *Runner) shouldInterrupt(st *checkpoint.State, step int) bool {
	return r.cfg.InterruptAfter > 0 && step == r.cfg.InterruptAfter &&
		st.Stage != checkpoint.StageComplete
}

// interrupt 持久化中断态并返回哨兵错误。
func (r *Runner) interrupt(st *checkpoint.State, start time.Time) error {
	_ = r.store.Save(st)
	return fmt.Errorf("阶段 %s 后受控中断（已运行 %dms）: %w",
		st.Stage, time.Since(start).Milliseconds(), model.ErrInterrupted)
}

// IsInterrupted 判断错误是否为受控中断。
func IsInterrupted(err error) bool { return errors.Is(err, model.ErrInterrupted) }
