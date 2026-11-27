// Package pipeline 编排解复用、映射、时间线、流控与转封装的完整处理内核，
// 并负责检查点安全点、模拟中断与确定性恢复。
package pipeline

import (
	"context"
	"errors"
	"io"
	"os"

	"relay/internal/checkpoint"
	"relay/internal/demux"
	"relay/internal/flow"
	"relay/internal/mapping"
	"relay/internal/media"
	"relay/internal/remux"
	"relay/internal/timeline"
)

// ErrInterrupted 表示达到 --interrupt-after 阈值的可恢复模拟中断。
var ErrInterrupted = errors.New("simulated interrupt threshold reached")

// Options 是一次 run 的全部输入。
type Options struct {
	InputPath      string
	OutputPath     string
	MapPath        string
	CheckpointPath string
	ManifestPath   string
	MaxBuffer      int64 // 总在途字节硬上限
	InterruptAfter int   // 消费到该帧数后在安全点中断；<=0 表示不中断
}

// Stats 汇总一次运行/恢复的可观测指标。
type Stats struct {
	Consumed     int64
	Released     int64
	Duplicates   int64
	Quarantined  int64
	PeakBuffer   int64
	ResumeMS     int64
	ResumedFrame int64
}

// Engine 持有一次处理的全部协作组件。
type Engine struct {
	opts        Options
	mapper      *mapping.Mapper
	dedup       *mapping.Dedup
	tl          *timeline.Engine
	pipe        *flow.Pipe
	writer      *remux.Writer
	ckpt        *checkpoint.Data
	stats       Stats
	safeOffset  int64          // 所有已释放帧之后的最小安全输入偏移
	pendingSeen *seenSequences // 回溯区在途的 (轨道,序号)，防止重复入线
}

// seenSequences 是回溯区在途帧序号的轻量集合。
type seenSequences struct{ m map[uint32]map[uint64]bool }

func newSeenSequences() *seenSequences {
	return &seenSequences{m: map[uint32]map[uint64]bool{}}
}

func (s *seenSequences) has(track uint32, seq uint64) bool {
	return s.m[track] != nil && s.m[track][seq]
}

func (s *seenSequences) add(track uint32, seq uint64) {
	if s.m[track] == nil {
		s.m[track] = map[uint64]bool{}
	}
	s.m[track][seq] = true
}

func (s *seenSequences) remove(track uint32, seq uint64) {
	if s.m[track] != nil {
		delete(s.m[track], seq)
	}
}

// New 按选项装配引擎；resume 为 false 时建立全新输出与检查点基线。
func New(ctx context.Context, opts Options, resume bool) (*Engine, error) {
	cfg, err := mapping.LoadConfig(opts.MapPath)
	if err != nil {
		return nil, err
	}
	mapper, err := mapping.NewMapper(cfg)
	if err != nil {
		return nil, err
	}
	format, err := remux.ParseFormat(opts.OutputPath)
	if err != nil {
		return nil, err
	}
	// 管道占用约 3/4 总预算，其余为解析与回溯区工作集，合计不越界。
	pipeCap := opts.MaxBuffer - opts.MaxBuffer/4
	e := &Engine{
		opts:        opts,
		mapper:      mapper,
		dedup:       mapping.NewDedup(),
		tl:          timeline.NewEngine(media.GraceWindow),
		pipe:        flow.NewPipe(pipeCap),
		pendingSeen: newSeenSequences(),
	}

	if resume {
		// 中断阈值只属于首次模拟运行；恢复后必须跑到 EOF。
		e.opts.InterruptAfter = 0
		if err := e.restore(format); err != nil {
			return nil, err
		}
	} else {
		e.writer, err = remux.NewWriter(opts.OutputPath, format)
		if err != nil {
			return nil, err
		}
		if err := e.initFingerprints(format); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// Run 驱动完整处理直至 EOF、中断或错误。
func (e *Engine) Run(ctx context.Context) error {
	in, err := os.Open(e.opts.InputPath)
	if err != nil {
		return media.NewFault(media.StageConfig, 0, 0, 0, "open input: %v", err)
	}
	defer in.Close()
	if e.ckpt.InputOffset > 0 {
		if _, err := in.Seek(e.ckpt.InputOffset, io.SeekStart); err != nil {
			return media.NewFault(media.StageCheckpoint, 0, 0, 0, "seek input: %v", err)
		}
	}
	reader := demux.NewMFSReader(in, e.opts.InputPath)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame, err := reader.Read()
		if err != nil {
			return err // 含媒体损坏 Fault，由上层映射为退出码 2
		}
		if frame == nil {
			break // 输入 EOF
		}
		e.stats.Consumed++
		if err := e.classifyAndIngest(ctx, frame); err != nil {
			return err
		}
		// 达到释放阈值即在最近的提交之后中断。检查点记录 safeOffset：
		// 该偏移之前的帧全部已落盘并固化去重；其后在途的 pending 帧
		// 恢复时重放，因账本未固化而重新 accept，绝不丢失。
		if e.opts.InterruptAfter > 0 && int(e.stats.Released) >= e.opts.InterruptAfter {
			if err := e.checkpoint(e.safeOffset); err != nil {
				return err
			}
			e.shutdown()
			return ErrInterrupted
		}
	}

	// EOF：排空回溯区并提交剩余帧。
	for _, env := range e.tl.Flush() {
		if err := e.commitFrame(ctx, env); err != nil {
			return err
		}
	}
	e.stats.Quarantined = int64(len(e.tl.Quarantine()))
	if err := e.checkpoint(reader.Offset()); err != nil {
		return err
	}
	e.stats.PeakBuffer = e.pipe.PeakBytes()
	return e.writer.Close(e.opts.ManifestPath)
}

// classifyAndIngest 完成映射、去重判定与时间线摄入，释放帧即时提交。
func (e *Engine) classifyAndIngest(ctx context.Context, frame *media.Frame) error {
	trackID, corrected, err := e.mapper.Resolve(frame)
	if err != nil {
		return err
	}
	frame.EventNS = corrected
	// 先查回溯区在途序号：同一 (track,seq) 尚未落盘就再次到达，
	// 按幂等重复处理（避免重复进入时间线）。
	if e.pendingSeen.has(trackID, frame.Sequence) {
		e.stats.Duplicates++
		return nil
	}
	decision := e.dedup.Peek(trackID, frame)
	switch decision.Kind {
	case mapping.DecisionDuplicate:
		e.stats.Duplicates++
		return nil // 已提交过的相同帧，幂等忽略
	case mapping.DecisionConflict:
		e.tl.QuarantineExplicit(&timeline.Envelope{TrackID: trackID, Frame: frame},
			timeline.ReasonDuplicateConflict, "same sequence with different payload hash")
		e.stats.Quarantined++
		return nil
	}
	env := &timeline.Envelope{TrackID: trackID, Frame: frame}
	e.pendingSeen.add(trackID, frame.Sequence)
	for _, released := range e.tl.Ingest(env) {
		if err := e.commitFrame(ctx, released); err != nil {
			return err
		}
	}
	return nil
}

// commitFrame 通过有界管道形成背压后写出，并归还字节额度。
func (e *Engine) commitFrame(ctx context.Context, env *timeline.Envelope) error {
	if err := e.pipe.Send(ctx, env.Frame); err != nil {
		return err
	}
	if _, err := e.writer.WriteFrame(env); err != nil {
		e.pipe.Release(env.Frame)
		return err
	}
	e.pipe.Ack(env.Frame)
	// 帧已确定落盘：固化去重账本、移出在途集合、推进安全输入偏移。
	e.dedup.Commit(env.TrackID, env.Frame.Sequence, env.Frame.SHA256)
	e.pendingSeen.remove(env.TrackID, env.Frame.Sequence)
	if next := env.Frame.SourceOffset + int64(env.Frame.RecordLen()) + media.PrefixLen; next > e.safeOffset {
		e.safeOffset = next
	}
	e.ckpt.FramesReleased++
	e.stats.Released++
	return nil
}

// shutdown 在中断路径释放管道额度与关闭文件句柄（清单留给恢复后最终写）。
func (e *Engine) shutdown() {
	e.pipe.Close()
	_ = e.writer.CloseQuiet()
}

// Stats 返回当前指标快照。
func (e *Engine) Stats() Stats {
	s := e.stats
	s.PeakBuffer = e.pipe.PeakBytes()
	return s
}
