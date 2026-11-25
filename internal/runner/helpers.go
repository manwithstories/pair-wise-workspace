package runner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"relaycheck/internal/index"
	"relaycheck/internal/model"
	"relaycheck/internal/pipeline"
	"relaycheck/internal/report"
)

func validate(cfg Config) error {
	if cfg.InputDir == "" || cfg.IndexPath == "" {
		return errors.New("input and index paths are required")
	}
	if cfg.Chunk <= 0 {
		return errors.New("--chunk must be positive")
	}
	if cfg.Queue <= 0 {
		return errors.New("--queue must be positive")
	}
	if cfg.MemBytes <= 0 {
		return errors.New("--mem must be positive")
	}
	if cfg.PauseAfter < 0 {
		return errors.New("--pause-after must be non-negative")
	}
	return nil
}

// rtpFileFor 把分片文件名映射为同名 .rtp 诊断记录。
func rtpFileFor(fragFile string) string {
	for i := len(fragFile) - 1; i >= 0; i-- {
		if fragFile[i] == '.' {
			return fragFile[:i] + ".rtp"
		}
	}
	return fragFile + ".rtp"
}

// encodeFragment 生成确定性的分片输出字节，纳入检查点输出哈希链。
func encodeFragment(rep model.FragmentReport) []byte {
	b, _ := json.Marshal(struct {
		Seq       int   `json:"seq"`
		Samples   int   `json:"samples"`
		FirstBad  int64 `json:"first_bad"`
		MDATBytes int64 `json:"mdat_bytes"`
	}{rep.Seq, len(rep.Samples), rep.FirstBadOff, rep.BytesInMDAT})
	return b
}

func seqsThrough(cross model.ManifestReport, through int) []int {
	var out []int
	for _, m := range cross.Segments {
		if m.Seq <= through {
			out = append(out, m.Seq)
		}
	}
	return out
}

func hitPause(cfg Config, seq int) bool {
	return cfg.PauseAfter > 0 && seq == cfg.PauseAfter
}

// pause 在块边界落盘检查点后，排空流水线并返回退出码 75 的暂停结果。
func pause(p *pipeline.Pipeline, store *index.Store, chunkID, nextSeq int,
	cross model.ManifestReport, builder *report.Builder, mem *model.MemTracker,
	resumeStart time.Time,
) Result {
	if _, err := store.Save(chunkID, nextSeq, seqsThrough(cross, nextSeq-1)); err != nil {
		return Result{ExitCode: ExitIOFailure}
	}
	p.Close()
	sum := builder.Finalize(mem.Peak(), elapsedMS(resumeStart), store.OutputSHA())
	return Result{Summary: sum, ExitCode: ExitPaused, Paused: true}
}

// elapsedMS 返回续跑耗时（毫秒）；非续跑（零值时间）返回 0。
// 亚毫秒的续跑向上取整为 1ms，确保终端打印的是真实的非零毫秒值。
func elapsedMS(start time.Time) int64 {
	if start.IsZero() {
		return 0
	}
	ms := time.Since(start).Milliseconds()
	if ms == 0 {
		return 1
	}
	return ms
}

// runGuard 确保 ctx 取消时取消流水线（供未来超时/信号扩展）。
func runGuard(ctx context.Context, p *pipeline.Pipeline) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			p.Cancel()
		case <-done:
		}
	}()
	return func() { close(done) }
}
