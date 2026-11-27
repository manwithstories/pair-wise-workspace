package pipeline

import (
	"context"
	"os"

	"relay/internal/demux"
	"relay/internal/mapping"
	"relay/internal/media"
	"relay/internal/timeline"
)

// produce 在生产者 goroutine 中解复用、映射、去重判定并把释放帧投入管道。
// 时间线、pendingSeen 仅由本 goroutine 访问，无需加锁；stats/safeOffset
// 与消费者共享，通过 Engine.mu 保护。
func (e *Engine) produce(ctx context.Context, in *os.File, cancel context.CancelFunc) error {
	// 续跑时底层文件已 seek 到 InputOffset，reader 从同一基准累计真实偏移。
	reader := demux.NewMFSReaderAt(in, e.opts.InputPath, e.ckpt.InputOffset)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame, err := reader.Read()
		if err != nil {
			cancel()   // 通知消费者尽快停止
			return err // 含媒体损坏 Fault，由上层映射为退出码 2
		}
		if frame == nil {
			break // 输入 EOF
		}
		e.mu.Lock()
		e.stats.Consumed++
		e.mu.Unlock()
		if err := e.classifyAndIngest(ctx, frame); err != nil {
			cancel()
			return err
		}
		// 达到处理阈值后标记中断请求。阈值按生产者同步计数判定，避免与
		// 异步消费者的落盘计数竞争。仍把回溯区在途帧全部排空投递，保证已
		// 读入帧不丢失；真正的检查点在 Run 等待消费者排空后建立。
		if e.opts.InterruptAfter > 0 && e.consumedCount() >= int64(e.opts.InterruptAfter) {
			e.markInterrupt()
			for _, env := range e.tl.Flush() {
				if err := e.dispatch(ctx, env); err != nil {
					cancel()
					return err
				}
			}
			break
		}
	}

	// 正常 EOF：把回溯区剩余帧全部确定性排出并投递。
	if !e.interruptFlag() {
		for _, env := range e.tl.Flush() {
			if err := e.dispatch(ctx, env); err != nil {
				cancel()
				return err
			}
		}
	}
	return nil
}

// consume 在消费者 goroutine 中从有界管道取帧、写盘并固化账本。
// 管道关闭且排空后 Recv 返回 nil，goroutine 退出。
func (e *Engine) consume(ctx context.Context, errCh chan<- error) {
	for {
		env, ok, err := e.pipe.Recv(ctx)
		if err != nil {
			errCh <- err
			return
		}
		if !ok {
			errCh <- nil
			return
		}
		if _, werr := e.writer.WriteFrame(env); werr != nil {
			e.pipe.Release(env)
			errCh <- werr
			return
		}
		e.pipe.Ack(env)
		e.onCommitted(env)
	}
}

// classifyAndIngest 完成映射、去重判定与时间线摄入。
func (e *Engine) classifyAndIngest(ctx context.Context, frame *media.Frame) error {
	trackID, corrected, err := e.mapper.Resolve(frame)
	if err != nil {
		return err
	}
	frame.EventNS = corrected

	e.mu.Lock()
	dupSeen := e.pendingSeen.has(trackID, frame.Sequence)
	decision := e.dedup.Peek(trackID, frame)
	e.mu.Unlock()

	if dupSeen {
		e.addDuplicate()
		return nil
	}
	switch decision.Kind {
	case mapping.DecisionDuplicate:
		e.addDuplicate()
		return nil // 已提交过的相同帧，幂等忽略
	case mapping.DecisionConflict:
		e.mu.Lock()
		e.tl.QuarantineExplicit(&timeline.Envelope{TrackID: trackID, Frame: frame},
			timeline.ReasonDuplicateConflict, "same sequence with different payload hash")
		e.stats.Quarantined++
		e.mu.Unlock()
		return nil
	}

	env := &timeline.Envelope{TrackID: trackID, Frame: frame}
	e.mu.Lock()
	e.pendingSeen.add(trackID, frame.Sequence)
	e.mu.Unlock()

	for _, released := range e.tl.Ingest(env) {
		if err := e.dispatch(ctx, released); err != nil {
			return err
		}
	}
	return nil
}

// dispatch 经有界管道投递一帧；额度不足时阻塞形成背压。
func (e *Engine) dispatch(ctx context.Context, env *timeline.Envelope) error {
	return e.pipe.Send(ctx, env)
}

// onCommitted 在消费者成功写盘后固化去重、推进安全偏移与计数。
func (e *Engine) onCommitted(env *timeline.Envelope) {
	next := env.Frame.SourceOffset + int64(env.Frame.RecordLen()) + media.PrefixLen
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dedup.Commit(env.TrackID, env.Frame.Sequence, env.Frame.SHA256)
	e.pendingSeen.remove(env.TrackID, env.Frame.Sequence)
	if next > e.safeOffset {
		e.safeOffset = next
	}
	e.stats.Released++
}

// --- 共享状态的加锁访问器 ---

func (e *Engine) addDuplicate() {
	e.mu.Lock()
	e.stats.Duplicates++
	e.mu.Unlock()
}

func (e *Engine) releasedCount() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stats.Released
}

func (e *Engine) consumedCount() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stats.Consumed
}

func (e *Engine) markInterrupt() {
	e.mu.Lock()
	e.interruptRequested = true
	e.mu.Unlock()
}

func (e *Engine) interruptFlag() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.interruptRequested
}

// interruptRequested 字段挂在 Engine 上，由 markInterrupt 设置。
func (e *Engine) safeOffsetSnapshot() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.safeOffset
}

func (e *Engine) inputSizeSnapshot() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 正常完成时输入已全部处置，安全偏移即输入总长度。
	return e.ckpt.InputSize
}

// Stats 返回当前指标快照。
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.stats
	s.PeakBuffer = e.pipe.PeakBytes()
	return s
}
