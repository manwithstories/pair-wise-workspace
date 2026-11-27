package pipeline

import (
	"os"
	"path/filepath"

	"medpipe/internal/clock"
	"medpipe/internal/index"
	"medpipe/internal/ingest"
	"medpipe/internal/timeline"
)

// Run 执行完整流水线；Resume=true 时先校验检查点并跳过已提交块。
func (r *Runner) Run() (*Result, error) {
	if err := os.MkdirAll(r.opts.OutDir, 0o755); err != nil {
		return nil, clock.WrapError(clock.CodeInvalidParam, -1, err, "创建输出目录失败")
	}
	cp := newCheckpoint(r.params())
	startBlock := int64(0)
	if r.opts.Resume {
		loaded, err := loadCheckpoint(r.opts.OutDir, r.params())
		if err != nil {
			return nil, err
		}
		cp = loaded
		startBlock = cp.LastBlock + 1
		if !cp.Interrupted {
			return nil, clock.NewError(clock.CodeCheckpoint, -1, "检查点未标记中断，无需 resume")
		}
	}

	// 扫描（内容变化自动作废旧索引重建）；坏头/半帧在此直接失败，不出清单。
	idx, _, err := index.LoadOrRebuild(r.opts.InputDir, filepath.Join(r.opts.OutDir, "index-cache.json"), r.opts.MaxBlock)
	if err != nil {
		return nil, err
	}
	totalFrames := r.tb.SamplesForDuration(clock.MustRational(int64(r.opts.Seconds), 1))
	r.mixer, err = timeline.NewMixer(r.tb, r.opts.Tracks, 16, totalFrames)
	if err != nil {
		return nil, err
	}
	// 重排窗口必须容纳一整个块的包，否则块内突发丢失会让后续包越窗。
	pktsPerBlock := (idx.Blocks[0].Frames + r.opts.PacketFrames - 1) / r.opts.PacketFrames
	window := r.opts.WindowSize
	if window < pktsPerBlock+1 {
		window = pktsPerBlock + 1
	}
	r.window = window
	cfg := ingest.Config{
		WindowSize:     window,
		MaxBufferBytes: int(window) * r.opts.Tracks * 2 * int(r.opts.PacketFrames) * 2,
		PacketFrames:   r.opts.PacketFrames,
		TotalFrames:    totalFrames,
		BytesPerFrame:  r.opts.Tracks * 2,
	}
	ro, err := ingest.New(cfg)
	if err != nil {
		return nil, err
	}
	streamer, err := ingest.NewStreamer(idx, ro, r.opts.PacketFrames)
	if err != nil {
		return nil, err
	}

	res := &Result{Checkpoint: cp}
	// resume 场景：先重放检查点覆盖的包，保证 mixer 状态与完整运行一致。
	if r.opts.Resume {
		if err := r.replay(cp, idx, ro, streamer); err != nil {
			return nil, err
		}
	}
	if err := r.processBlocks(idx, ro, streamer, cp, startBlock, res); err != nil {
		return res, err
	}
	if err := r.drainAndFill(idx, ro, cp); err != nil {
		return res, err
	}
	return r.finalize(cp, res)
}
