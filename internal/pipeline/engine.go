// Package pipeline 编排解复用、映射、时间线、流控与转封装的完整处理内核，
// 并负责检查点安全点、模拟中断与确定性恢复。
package pipeline

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"relay/internal/checkpoint"
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
	InterruptAfter int   // 提交该帧数后模拟中断；<=0 表示不中断
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
	pipe        *flow.Pipe[*timeline.Envelope]
	writer      *remux.Writer
	ckpt        *checkpoint.Data
	pendingSeen *seenSequences

	mu                 sync.Mutex // 保护 stats、safeOffset 与上述账本的跨 goroutine 访问
	stats              Stats
	safeOffset         int64
	interruptRequested bool
}

// seenSequences 记录回溯区在途 (轨道,序号) 的载荷哈希，仅生产者 goroutine 访问。
// 同序号同哈希幂等忽略，哈希不同即使尚未落盘也按冲突隔离，不得静默覆盖。
type seenSequences struct {
	m map[uint32]map[uint64][32]byte
}

func newSeenSequences() *seenSequences {
	return &seenSequences{m: map[uint32]map[uint64][32]byte{}}
}

// lookup 返回在途哈希与是否存在该序号。
func (s *seenSequences) lookup(track uint32, seq uint64) ([32]byte, bool) {
	if ledger := s.m[track]; ledger != nil {
		h, ok := ledger[seq]
		return h, ok
	}
	var zero [32]byte
	return zero, false
}

func (s *seenSequences) add(track uint32, seq uint64, hash [32]byte) {
	if s.m[track] == nil {
		s.m[track] = map[uint64][32]byte{}
	}
	s.m[track][seq] = hash
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
		opts:   opts,
		mapper: mapper,
		dedup:  mapping.NewDedup(),
		tl:     timeline.NewEngine(media.GraceWindow),
		pipe: flow.NewPipe[*timeline.Envelope](pipeCap, func(env *timeline.Envelope) int64 {
			return flow.FrameCost(env.Frame)
		}),
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
// 生产者 goroutine 解复用/映射/入线并经有界管道投递；消费者 goroutine
// 写出落盘并固化账本，慢盘通过管道字节额度对生产者形成背压。
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

	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	consumeErr := make(chan error, 1)
	go e.consume(childCtx, consumeErr)

	producerErr := e.produce(childCtx, in, cancel)
	// 无论生产者正常结束还是出错，都关闭管道并等待消费者排空/退出。
	e.pipe.Close()
	if cerr := <-consumeErr; cerr != nil && producerErr == nil {
		producerErr = cerr
	}
	if producerErr != nil {
		return producerErr
	}

	// 到此处消费者已排空全部投递帧，落盘与账本固化均完成，是安全点。
	e.mu.Lock()
	e.stats.Quarantined = int64(len(e.tl.Quarantine()))
	e.stats.PeakBuffer = e.pipe.PeakBytes()
	e.mu.Unlock()

	if e.interruptFlag() {
		if err := e.checkpoint(e.safeOffsetSnapshot()); err != nil {
			return err
		}
		_ = e.writer.CloseQuiet()
		return ErrInterrupted
	}
	if err := e.checkpoint(e.inputSizeSnapshot()); err != nil {
		return err
	}
	return e.writer.Close(e.opts.ManifestPath)
}
