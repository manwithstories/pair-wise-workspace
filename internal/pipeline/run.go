// Package pipeline 编排输入校验、时间线恢复、分片、码率扇出/汇合与检查点续跑。
// 数据流严格按 输入校验 -> 时间线恢复 -> 分片与扇出 -> 边界核验 单向推进，
// 全部时间以整数纳秒计算，所有分片边界取自共享时钟。
package pipeline

import (
	"encoding/hex"
	"errors"

	"archivecheck/internal/checkpoint"
	"archivecheck/internal/fixture"
	"archivecheck/internal/ingest"
	"archivecheck/internal/model"
	"archivecheck/internal/transport"
)

// Config 是一次验收运行的全部参数。
type Config struct {
	Params          fixture.Params
	SegmentTargetNS int64
	BoundaryTolNS   int64
	WindowSize      int
	Fault           transport.FaultConfig
	CheckpointPath  string
	// 跨模块故障场景：时钟突变与表示层边界冲突同时注入。
	CrossModuleFault bool
	// TailTruncate 截掉末尾若干帧，制造不完整尾分片样例。
	TailTruncate bool
}

// Report 是验收结论。Published=false 时清单哈希为空，调用方必须非零退出。
type Report struct {
	OK             bool
	Published      bool
	SyncErrorNS    int64
	RecoveryRate   float64
	P95LatencyNS   int64
	Segments       int
	BoundaryMiss   int
	ManifestHash   string
	FirstError     *ingest.FirstError
	Gaps           []model.Gap
	CheckpointUsed bool
	RejectedReason string
	ManifestByRep  map[string]model.Manifest
}

// ErrCheckpointMismatch 透传检查点不匹配错误，供入口映射退出码。
var ErrCheckpointMismatch = checkpoint.ErrDigestMismatch

func defaults(c Config) Config {
	if c.SegmentTargetNS <= 0 {
		c.SegmentTargetNS = 2_000_000_000
	}
	if c.BoundaryTolNS <= 0 {
		c.BoundaryTolNS = 100_000_000
	}
	if c.WindowSize <= 0 {
		c.WindowSize = 64
	}
	return c
}

// Run 执行一次完整的确定性恢复与验收。
func Run(c Config) (Report, error) {
	c = defaults(c)
	rep := Report{ManifestByRep: map[string]model.Manifest{}}

	inSum := fixture.InputDigest(c.Params)
	inputDigest := hex.EncodeToString(inSum[:])
	pmSum := ParamsHash(c)
	paramsDigest := hex.EncodeToString(pmSum[:])
	if c.CheckpointPath != "" {
		if st, err := checkpoint.Load(c.CheckpointPath); err == nil {
			if err := checkpoint.Verify(st, inputDigest, paramsDigest); err != nil {
				return rep, err
			}
			rep.CheckpointUsed = true
		} else if !errors.Is(err, checkpoint.ErrNoCheckpoint) {
			return rep, err
		}
	}

	frames := fixture.Generate(c.Params)
	if c.CrossModuleFault {
		applyCrossModuleFault(frames, c.Params)
	}
	if c.TailTruncate {
		applyTailTruncate(frames)
	}

	if err := runTransport(c, frames, &rep); err != nil {
		return rep, err
	}
	results, err := runVerifyAndTimeline(c, frames, &rep)
	if err != nil || rep.FirstError != nil {
		return rep, err
	}
	if err := runSegmentAndMerge(c, frames, results, &rep); err != nil {
		return rep, err
	}
	if c.CheckpointPath != "" && rep.Published {
		_ = checkpoint.Remove(c.CheckpointPath)
	}
	return rep, nil
}
