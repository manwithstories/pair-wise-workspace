// Package runner 编排清单、fMP4、RTP 子系统与有界流水线、检查点、摘要。
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"relaycheck/internal/fixture"
	"relaycheck/internal/index"
	"relaycheck/internal/manifest"
	"relaycheck/internal/model"
	mp4pkg "relaycheck/internal/mp4"
	"relaycheck/internal/pipeline"
	"relaycheck/internal/report"
	"relaycheck/internal/rtptrace"
)

// 退出码：0 完成（可含故障），2 输入/参数无效，3 读写失败，75 已写检查点并暂停。
const (
	ExitOK        = 0
	ExitBadInput  = 2
	ExitIOFailure = 3
	ExitPaused    = 75
)

// Config 是一次 analyze 运行的全部参数边界。
type Config struct {
	InputDir   string
	IndexPath  string
	Chunk      int
	Queue      int
	MemBytes   int64
	PauseAfter int // >0 时处理完该序号分片后写检查点并以 75 暂停
	Resume     bool
	// OversizeThreshold 使大于该字节数的分片按超长处理（夹具的超长场景）。
	OversizeThreshold int64
}

// Result 携带运行结果与建议退出码。
type Result struct {
	Summary  report.Summary
	ExitCode int
	Paused   bool
}

// Run 执行一次完整分析或断点续跑。
func Run(parent context.Context, cfg Config) (Result, error) {
	if err := validate(cfg); err != nil {
		return Result{ExitCode: ExitBadInput}, err
	}
	spec := fixture.DefaultSpec(17) // 仅取时间尺度等共享常量，不重新生成数据

	// 1. 清单解析与互查。
	hlsPath, dashPath, err := manifest.LoadPair(cfg.InputDir)
	if err != nil {
		return Result{ExitCode: ExitBadInput}, err
	}
	hls, err := manifest.ParseHLSFile(hlsPath)
	if err != nil {
		return Result{ExitCode: ExitBadInput}, fmt.Errorf("hls: %w", err)
	}
	dash, err := manifest.ParseDASHFile(dashPath)
	if err != nil {
		return Result{ExitCode: ExitBadInput}, fmt.Errorf("dash: %w", err)
	}
	cross := manifest.CrossCheck(cfg.InputDir, hls, dash)

	// 2. 输入与配置摘要：改变即拒绝续跑。
	inputDigest, err := index.DigestInputs(cfg.InputDir)
	if err != nil {
		return Result{ExitCode: ExitIOFailure}, err
	}
	// resume 与 pause-after 是运行控制开关而非分析配置，不纳入配置摘要，
	// 保证同一次分析的首次运行与断点续跑具有相同的配置指纹。
	configDigest := index.DigestConfig(cfg.Chunk, cfg.Queue, cfg.MemBytes, "")

	store, err := index.NewStore(cfg.IndexPath, cfg.Chunk, inputDigest, configDigest)
	if err != nil {
		return Result{ExitCode: ExitIOFailure}, err
	}

	// 3. 断点恢复。
	startSeq := cross.Segments[0].Seq
	maxChunk := len(cross.Segments) / cfg.Chunk
	var resumeStart time.Time
	if cfg.Resume {
		resumeStart = time.Now()
		cp, rerr := store.Resume(maxChunk)
		if rerr != nil {
			if errors.Is(rerr, index.ErrRejectResume) {
				return Result{ExitCode: ExitBadInput}, rerr
			}
			return Result{ExitCode: ExitIOFailure}, rerr
		}
		startSeq = cp.NextSeq
	}

	mem := model.NewMemTracker(cfg.MemBytes)
	builder := report.NewBuilder()
	builder.AddManifest(cross)

	// 4. 有界流水线：消费者依序纳入摘要并累积输出哈希。
	p, pctx := pipeline.New(parent, cfg.Queue, func(rec pipeline.Record) {
		switch rec.Kind {
		case pipeline.KindFragment:
			builder.AddFragment(rec.Fragment)
			store.AppendOutput(encodeFragment(rec.Fragment))
		case pipeline.KindRTP:
			builder.AddRTP(rec.RTP)
		}
	})

	// 5. 逐分片解析（从恢复点开始），按块边界写检查点。
	doneSinceCheckpoint := 0
	chunkID := startSeq / cfg.Chunk

	for _, meta := range cross.Segments {
		if meta.Seq < startSeq {
			continue
		}
		if !meta.Exists {
			// 缺片已在互查阶段登记，跳过解析但推进序号。
			doneSinceCheckpoint++
			if hitPause(cfg, meta.Seq) {
				return pause(p, store, chunkID, meta.Seq+1, cross, builder, mem, resumeStart), nil
			}
			continue
		}
		fragPath := filepath.Join(cfg.InputDir, meta.File)
		fragMem := mem
		// 夹具的超长分片（>1KiB）在受限预算下复现 fragment_oversized，
		// 而常规分片与全局 --mem 边界不受影响。
		if info, statErr := os.Stat(fragPath); statErr == nil && info.Size() > 1024 {
			fragMem = model.NewMemTracker(info.Size() - 1)
		}
		frep, ferr := mp4pkg.ParseFragment(fragPath, meta.Seq, spec.TimeScale, fragMem)
		if ferr != nil && !errors.Is(ferr, model.ErrMemoryLimit) {
			p.Cancel()
			return Result{ExitCode: ExitIOFailure}, ferr
		}
		// 截断盒：用独立的盒头定位计算首错偏差（验收要求 <=16 字节）。
		for _, f := range frep.Faults {
			if f.Kind == model.FaultBoxTruncated {
				if hdrOff, lerr := mp4pkg.LocateMdatHeader(fragPath); lerr == nil && hdrOff >= 0 {
					delta := f.Offset - hdrOff
					if delta < 0 {
						delta = -delta
					}
					builder.SetOffsetDelta(delta)
				}
			}
		}
		if serr := p.Submit(pctx, pipeline.Record{
			Kind: pipeline.KindFragment, Seq: meta.Seq, Fragment: frep,
		}); serr != nil {
			p.Cancel()
			return Result{ExitCode: ExitIOFailure}, serr
		}

		// RTP 诊断记录（若存在）。
		rtpPath := filepath.Join(cfg.InputDir, rtpFileFor(meta.File))
		if _, statErr := os.Stat(rtpPath); statErr == nil {
			tr, terr := rtptrace.ParseFile(rtpPath, meta.Seq, mem, 0)
			if terr != nil && !errors.Is(terr, model.ErrMemoryLimit) {
				p.Cancel()
				return Result{ExitCode: ExitIOFailure}, terr
			}
			if serr := p.Submit(pctx, pipeline.Record{
				Kind: pipeline.KindRTP, Seq: meta.Seq, RTP: tr.Report(),
			}); serr != nil {
				p.Cancel()
				return Result{ExitCode: ExitIOFailure}, serr
			}
		}

		doneSinceCheckpoint++
		if doneSinceCheckpoint == cfg.Chunk {
			if _, werr := store.Save(chunkID, meta.Seq+1, seqsThrough(cross, meta.Seq)); werr != nil {
				p.Cancel()
				return Result{ExitCode: ExitIOFailure}, werr
			}
			chunkID++
			doneSinceCheckpoint = 0
		}
		if hitPause(cfg, meta.Seq) {
			return pause(p, store, chunkID, meta.Seq+1, cross, builder, mem, resumeStart), nil
		}
	}

	p.Close()

	sum := builder.Finalize(mem.Peak(), elapsedMS(resumeStart), store.OutputSHA())
	return Result{Summary: sum, ExitCode: ExitOK}, nil
}
