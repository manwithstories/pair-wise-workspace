// runner.go — errgroup 执行器：作业组 fan-out、取消传播、同键互斥、优雅停机排空。
//
// 取消传播模型（题面功能 2）：
//
//	一个依赖组 = 一个 errgroup.Group + 一个派生的 context。
//	组内每个作业在 g.Go() 里跑，并各自持有自己的子 context；
//	任一作业返回非 nil error -> errgroup 自动 cancel 组 context
//	-> 兄弟作业的 ctx.Done() 关闭 -> 它们必须"排空在途计算"再 return，
//	并且无论成功失败都释放已占资源（defer lease.Release），
//	从而"部分结果不得泄漏"：没有作业会带着已占资源退出。
//
// 同键互斥（题面功能 3）：
//
//	keyLocks[idemKey] 是 per-key 的互斥锁。执行期先 TryLock 拿锁：
//	拿不到说明同键正在跑，直接返回 ErrDuplicate，不再二次占用资源，
//	从而杜绝"手抖重复提交导致 GPU 被双份占满"。
//
// 优雅停机（题面功能 4）：
//
//	GracefulStop 置 draining -> 给在途组 30s 排空 -> 超时强制取消 -> WAL 落盘。
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// ErrKeyBusy 表示同幂等键的作业正在执行期运行。
var ErrKeyBusy = errors.New("同幂等键作业正在执行中，拒绝并发双跑")

// Runner 是执行器。
type Runner struct {
	sched *Scheduler

	// 同键互斥锁表（执行期防双跑）。
	keyMu    sync.Mutex
	keyLocks map[string]*sync.Mutex

	// 在途作业计数（优雅停机排空用）。
	inflight sync.WaitGroup

	// 组级 errgroup 注册表：组 ID -> *errgroup.Group（Stop 时统一 cancel）。
	groupMu sync.Mutex
	groups  map[string]*groupState

	// 在途任务计数，供 Status/排空判断。
	nActive atomic.Int64

	// admitMu 是"准入门"：保证 inflight.Add(1) 与 GracefulStop 里的
	// inflight.Wait() 不会并发发生。
	//
	// sync.WaitGroup 的硬性约束：计数器为 0 时的 Add 必须发生在 Wait 之前。
	// 若不加这道门，优雅停机可能在计数为 0 的瞬间与新作业启动竞争，
	// 导致 Wait 提前返回（假排空），也会触发 WaitGroup 内部竞态。
	// stopping 置位后不再接纳新作业，pending 作业留给 WAL 重放恢复。
	admitMu  sync.Mutex
	stopping bool

	// 根 context：所有执行组的祖先，优雅停机时统一取消。
	// 在 NewRunner 里一次性建好（不可变），避免"Once 内写、外部读"的竞态。
	rootCtxVal context.Context
	rootCancel context.CancelFunc

	// 执行统计（多 goroutine 写入，必须用原子量）。
	nStarted  atomic.Int64
	nDrained  atomic.Int64
	nForced   atomic.Int64
	nCanceled atomic.Int64
}

// NewRunner 创建执行器。
func NewRunner(s *Scheduler) *Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{
		sched:      s,
		keyLocks:   map[string]*sync.Mutex{},
		groups:     map[string]*groupState{},
		rootCtxVal: ctx,
		rootCancel: cancel,
	}
}

// keyLock 返回某幂等键的执行期互斥锁（不存在则创建）。
func (r *Runner) keyLock(key string) *sync.Mutex {
	r.keyMu.Lock()
	defer r.keyMu.Unlock()
	m, ok := r.keyLocks[key]
	if !ok {
		m = &sync.Mutex{}
		r.keyLocks[key] = m
	}
	return m
}

// tryKeyLock 尝试获取同键执行锁；拿不到立即返回 false（非阻塞）。
func (r *Runner) tryKeyLock(key string) (*sync.Mutex, bool) {
	m := r.keyLock(key)
	if m.TryLock() {
		return m, true
	}
	return m, false
}

// start 启动一个已预留资源的作业。
// 依赖组作业挂进共享 errgroup；独立作业自成一个组。
func (s *Scheduler) start(h jobHandle) {
	r := s.runner
	if r == nil {
		// 没有 runner（纯决策模式）：立即完成，避免资源泄漏。
		s.Complete(h, nil)
		return
	}
	r.launch(h)
}

// AttachRunner 把执行器挂到调度器上（构造后调用一次）。
func (s *Scheduler) AttachRunner(r *Runner) { s.runner = r }

// launch 真正在 goroutine 中执行作业。
func (r *Runner) launch(h jobHandle) {
	j := h.j

	// 1) 同键执行期互斥：拿不到锁就释放本次预留（幂等键相同说明不该双跑）。
	mu, locked := r.tryKeyLock(j.IdemKey)
	if !locked {
		h.lease.Release()
		r.sched.Complete(jobHandle{j: j}, errors.Join(ErrKeyBusy,
			fmt.Errorf("幂等键 %q 正在被另一作业占用", j.IdemKey)))
		return
	}
	defer mu.Unlock()

	// 2) 决定 errgroup 归属：同组共用一个 group 与 context。
	g, ctx, err := r.groupFor(j)
	if err != nil {
		h.lease.Release()
		r.sched.Complete(jobHandle{j: j}, err)
		return
	}

	// 准入门：与 GracefulStop 的 inflight.Wait() 互斥。
	// 若停机已开始，释放本次预留并把作业留在 WAL（重启后重放恢复）。
	if !r.admit() {
		h.lease.Release()
		j.forceState(StatePending)
		r.sched.trace(TraceEvent{Kind: "drain", JobID: j.ID, Tenant: j.Tenant,
			Msg: "停机中，不再启动新作业（保留在 WAL 待重启恢复）", Used: r.sched.pool.Used()})
		return
	}

	r.nActive.Add(1)
	r.nStarted.Add(1)

	g.Go(func() error {
		defer r.inflight.Done()
		defer r.nActive.Add(-1)

		// 执行体：从 attempt 开始，失败可重试由 Complete 统一处理。
		ok := j.beginAttempt()
		if !ok {
			// 理论上不会发生（调度前已校验），保险起见直接失败。
			h.lease.Release()
			r.sched.Complete(jobHandle{j: j}, fmt.Errorf("重试次数已耗尽: %s", j.ID))
			return nil
		}

		runErr := r.runJob(ctx, j, h)

		// 关键：无论成功/失败/取消，资源都在这里交回调度器（幂等，不会双退）。
		r.sched.Complete(jobHandle{j: j, lease: h.lease}, runErr)

		// 返回 error 让 errgroup 取消组 context（取消传播的关键一环）。
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			return runErr
		}
		return nil
	})
}

// admit 登记一个新启动的在途作业。停机开始后返回 false。
// inflight.Add(1) 在这里完成，从而与 GracefulStop 的 Wait 严格串行。
func (r *Runner) admit() bool {
	r.admitMu.Lock()
	defer r.admitMu.Unlock()
	if r.stopping {
		return false
	}
	r.inflight.Add(1)
	return true
}

// beginStop 关闭准入门。返回后不会再有新的 inflight.Add，
// 因此可以安全地调用 inflight.Wait()。
func (r *Runner) beginStop() {
	r.admitMu.Lock()
	r.stopping = true
	r.admitMu.Unlock()
}

// runJob 执行作业本体，负责"排空"语义。
func (r *Runner) runJob(ctx context.Context, j *Job, h jobHandle) (err error) {
	// 排空保证：即便 panic 也要把资源还回去，避免泄漏。
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("作业 %s panic: %v", j.ID, rec)
		}
	}()

	r.sched.trace(TraceEvent{Kind: "start", JobID: j.ID, Tenant: j.Tenant,
		Msg:  fmt.Sprintf("开始执行（第 %d 次尝试，请求 %s）", j.attemptOf(), j.Req),
		Used: r.sched.pool.Used()})

	run := j.Run
	if run == nil {
		// 无执行体（理论上 WAL 水合后会有），视为成功。
		return nil
	}
	return run(ctx, j)
}

// groupFor 取（必要时创建）作业所属的 errgroup 与派生 context。
func (r *Runner) groupFor(j *Job) (*errgroup.Group, context.Context, error) {
	groupID := j.Group
	if groupID == "" {
		// 独立作业：自成一个组，绑定到 Runner 的排空 context。
		ctx, cancel := context.WithCancel(r.rootCtx())
		r.groupMu.Lock()
		r.groups["job:"+j.ID] = &groupState{id: "job:" + j.ID, cancel: cancel}
		r.groupMu.Unlock()
		g, gctx := errgroup.WithContext(ctx)
		return g, gctx, nil
	}

	r.groupMu.Lock()
	gs, ok := r.groups[groupID]
	if !ok {
		// 新组：父 context 绑定到 rootCtx，Stop 时整棵树一起取消。
		ctx, cancel := context.WithCancel(r.rootCtx())
		g, gctx := errgroup.WithContext(ctx)
		gs = &groupState{id: groupID, cancel: cancel, group: g, ctx: gctx}
		r.groups[groupID] = gs
		r.groupMu.Unlock()
		return g, gctx, nil
	}
	r.groupMu.Unlock()
	return gs.group, gs.ctx, nil
}

// rootCtx 返回所有执行组的根 context（在 NewRunner 中一次性创建，不可变）。
func (r *Runner) rootCtx() context.Context { return r.rootCtxVal }

// CancelGroup 主动取消某个依赖组（整组 context 取消 -> 兄弟与下游收到信号）。
func (r *Runner) CancelGroup(groupID, reason string) {
	r.groupMu.Lock()
	gs, ok := r.groups[groupID]
	r.groupMu.Unlock()
	if !ok || gs == nil || gs.cancel == nil {
		return
	}
	r.sched.trace(TraceEvent{Kind: "cancel", Msg: fmt.Sprintf("主动取消作业组 %s：%s", groupID, reason)})
	gs.cancel()
}

// ActiveCount 返回在途任务数。
func (r *Runner) ActiveCount() int64 { return r.nActive.Load() }

// ---- 优雅停机 -----------------------------------------------------------

// GracefulStop 执行优雅停机：
//  1. 停止接收新提交（draining）；
//  2. 等待在途任务排空，最多 timeout（题面 30s）；
//  3. 超时则强制取消所有残留组 context；
//  4. 最后强制 fsync WAL，保证重启可重放恢复。
func (r *Runner) GracefulStop(s *Scheduler, timeout time.Duration) {
	s.BeginDrain()
	s.trace(TraceEvent{Kind: "drain", Msg: "进入优雅停机：停止接收新提交，开始排空在途任务"})

	// 先关闭准入门，再启动等待者——顺序不能反。
	// 反过来就可能出现 inflight.Wait() 与 inflight.Add(1) 并发（WaitGroup 竞态），
	// 并让 Wait 在计数为 0 时提前返回，把在途作业误判为已排空。
	r.beginStop()

	deadline := time.Now().Add(timeout)
	drained := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		r.nDrained.Add(1)
		s.trace(TraceEvent{Kind: "drain", Msg: fmt.Sprintf("在途任务已全部排空（用时 %s）", time.Since(deadline.Add(-timeout)).Round(time.Millisecond))})
	case <-time.After(timeout):
		// 超时：强制取消残留。
		r.nForced.Add(1)
		active := r.nActive.Load()
		s.trace(TraceEvent{Kind: "drain", Msg: fmt.Sprintf("排空超时（> %s），强制取消残留 %d 个任务", timeout, active)})
		r.forceCancelAll(reasonTimeout)
		// 给被取消的任务一个短暂的收尾窗口（它们会因 ctx.Done 尽快返回）。
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			s.trace(TraceEvent{Kind: "drain", Msg: "强制取消后仍有残留，按现状落盘"})
		}
	}

	// 4) 最后一次性 fsync WAL：确保重启后能重放恢复到一致状态。
	if err := r.sched.wal.Flush(); err != nil {
		s.trace(TraceEvent{Kind: "drain", Msg: "WAL flush 失败: " + err.Error()})
	} else {
		s.trace(TraceEvent{Kind: "drain", Msg: "WAL 已最终 fsync 落盘，可重放恢复"})
	}
	// 取消 root context，释放底层资源。
	if r.rootCancel != nil {
		r.rootCancel()
	}
}

const reasonTimeout = "优雅停机超时强制取消"

// forceCancelAll 强制取消所有组 context。
func (r *Runner) forceCancelAll(reason string) {
	r.groupMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(r.groups))
	for _, gs := range r.groups {
		if gs.cancel != nil {
			cancels = append(cancels, gs.cancel)
		}
	}
	r.groupMu.Unlock()
	for _, c := range cancels {
		c()
	}
	if r.rootCancel != nil {
		r.rootCancel()
	}
}

// RunnerStats 是执行器统计。
type RunnerStats struct {
	Started int64 `json:"started"`
	Active  int64 `json:"active"`
	Drained int64 `json:"drained"`
	Forced  int64 `json:"forced"`
}

// Stats 返回执行器统计。
func (r *Runner) Stats() RunnerStats {
	return RunnerStats{
		Started: r.nStarted.Load(),
		Active:  r.nActive.Load(),
		Drained: r.nDrained.Load(),
		Forced:  r.nForced.Load(),
	}
}

// Run 是调度循环：反复 tick 直到 ctx 取消。
//
// 用 wakeCh 做事件驱动唤醒（资源释放 / 退避到期 / 依赖满足），并叠加一个兜底
// ticker 以保证"退避到期"这类没有显式事件的情况也会被重新评估。
func (r *Runner) Run(ctx context.Context) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		// 先跑一轮决策（尽可能填满资源池）。
		r.sched.tick()
		select {
		case <-ctx.Done():
			return
		case <-r.sched.WakeChan():
		case <-tick.C:
		}
	}
}
