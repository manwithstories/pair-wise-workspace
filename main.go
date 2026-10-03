package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// Engine 把调度器、资源池、执行器、WAL 四者缝在一起，并驱动主循环。
//
// 完整数据流（对应需求里的链路）：
//
//	提交 → 去重 → WAL 追加 → DRF 优先队列 → errgroup 执行组
//	     → 资源池扣减 → 回写 WAL
type Engine struct {
	pool   *Pool
	sched  *Scheduler
	wal    *WAL
	runner *Runner
	log    Logger

	// rootCtx 是所有作业组的父 context。停机时 cancel 它，
	// 所有 errgroup 一并收到取消信号。
	rootCtx    context.Context
	rootCancel context.CancelFunc

	// runFunc 是作业执行体。demo/bench 可注入自己的，默认为内置实现。
	runFunc RunFunc
	// recoveredCount 记录本次启动从 WAL 重放重建的作业数。
	recoveredCount int

	// tick 是主循环的决策间隔。选 5ms 是为了保证
	// 「单次 DRF 决策 <1ms」在实际 100 并发下仍有足够余量。
	tick time.Duration

	drainBudget time.Duration

	// submitCh 把提交请求串行化后送进主循环，保证「去重 → WAL → 入队」
	// 的顺序确定。
	submitCh chan submitReq

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
	// signalCh 是 SIGTERM/SIGINT 接收通道，由 Start 装好处理器后填充。
	signalCh chan os.Signal
	// noSignal 表示本引擎不接管进程信号（demo 里第二个引擎用），
	// 避免和先前的引擎抢同一个进程级 signal.Notify。
	noSignal bool

	// 排空结果（停机后读取）。drainer 是唯一写入方，drainDone 在它收尾后关闭；
	// Shutdown 必须等 drainDone，否则会读到零值（表现为 clean=false 且 detail 空）。
	drainDone   chan struct{}
	drainClean  bool
	drainDetail string
}

type submitReq struct {
	req   SubmitRequest
	reply chan submitReply
}

type submitReply struct {
	job     *Job
	created bool
	err     error
}

type EngineOptions struct {
	WALPath     string
	PoolTotal   Resources
	Sched       schedulerOptions
	Tick        time.Duration
	DrainBudget time.Duration
	Logger      Logger
	// WorkFunc 是作业执行体。demo 与真实部署在这里分流。
	WorkFunc RunFunc
}

func NewEngine(opts EngineOptions) (*Engine, error) {
	pool, err := NewPool(opts.PoolTotal)
	if err != nil {
		return nil, err
	}
	wal, records, err := OpenWAL(opts.WALPath)
	if err != nil {
		return nil, err
	}
	sched := NewScheduler(opts.Sched)
	sched.SetLogger(opts.Logger)
	e := &Engine{
		pool:        pool,
		sched:       sched,
		wal:         wal,
		log:         opts.Logger,
		tick:        opts.Tick,
		drainBudget: opts.DrainBudget,
		submitCh:    make(chan submitReq, 1024),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
		drainDone:   make(chan struct{}),
	}
	if e.tick <= 0 {
		e.tick = 5 * time.Millisecond
	}
	if e.drainBudget <= 0 {
		e.drainBudget = 30 * time.Second
	}
	e.rootCtx, e.rootCancel = context.WithCancel(context.Background())
	e.runner = NewRunner(pool, sched, wal)
	e.runner.SetLogger(opts.Logger)

	// WAL 重放：把崩溃时在途的作业原样重建，不需要人工重投。
	recovered := e.recoverFromWAL(records)
	e.recoveredCount = recovered
	if opts.Logger != nil && recovered > 0 {
		opts.Logger("RECOVER wal=%s replayed=%d records in_flight_recovered=%d truncated_tail=%dB",
			wal.Path(), wal.ReplayedRecords(), recovered, wal.TruncatedTail())
	}
	e.runFunc = opts.WorkFunc
	if e.runFunc == nil {
		e.runFunc = defaultWorkFunc
	}
	return e, nil
}

// logf 是引擎的日志出口，为 nil 时静音。
func (e *Engine) logf(format string, args ...any) {
	if e.log != nil {
		e.log(format, args...)
	}
}

func (e *Engine) Pool() *Pool         { return e.pool }
func (e *Engine) Sched() *Scheduler   { return e.sched }
func (e *Engine) WAL() *WAL           { return e.wal }
func (e *Engine) Runner() *Runner     { return e.runner }
func (e *Engine) RecoveredCount() int { return e.recoveredCount }

// Submit 提交一个作业。它是并发安全的：请求被送进主循环串行处理，
// 因此「同键只入队一次」在 100 并发下依然严格成立。
func (e *Engine) Submit(ctx context.Context, req SubmitRequest) (*Job, bool, error) {
	if err := req.Validate(); err != nil {
		return nil, false, err
	}
	reply := make(chan submitReply, 1)
	// 停机后立即拒绝：这条检查必须在把请求塞进通道之前，
	// 否则主循环已经退出、没人再消费 submitCh，请求会一直挂在缓冲区上，
	// 调用方也会永久阻塞（曾表现为停机后再提交一次就把进程挂死）。
	select {
	case <-e.stopCh:
		return nil, false, ErrDraining
	default:
	}
	select {
	case e.submitCh <- submitReq{req: req, reply: reply}:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-e.stopCh:
		return nil, false, ErrDraining
	}
	select {
	case r := <-reply:
		return r.job, r.created, r.err
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-e.stopCh:
		// 主循环已退出，不会再有人回复。
		return nil, false, ErrDraining
	}
}

// ErrDraining 表示调度器正在停机，已停止接收新提交。
var ErrDraining = errors.New("scheduler is draining, not accepting new submissions")

// ---------------------------------------------------------------------------
// WAL 重放恢复
// ---------------------------------------------------------------------------

// recoverFromWAL 把 WAL 记录重放回调度器。
//
// 恢复语义的关键判断：
//   - submitted 的作业，无论日志里有没有终结事件，都重建成 pending。
//     因为资源是在进程内存里扣减的，崩溃后已经全部释放，WAL 里也没有
//     资源分配信息。重建成 running 是错的（没有真实执行体在跑），
//     重建成 failed 也违背「不用人工重投」的需求——所以重建成 pending，
//     让 DRF 重新公平地排一次队。
//   - 依赖关系原样恢复，因此依赖组在重启后依然成立。
//   - 租户份额用 dispatched 事件里的账本快照预热，避免所有租户份额归零
//     后在重启瞬间集体抢跑。
func (e *Engine) recoverFromWAL(records []walRecord) int {
	if len(records) == 0 {
		return 0
	}
	e.sched.ResetForRecovery()

	// 第一遍之二：先记下哪些作业已经终结。这些作业绝不能被重放成待调度，
	// 否则每次重启都会把已经完成的活再跑一遍——对训练/评测来说就是重复烧卡。
	terminal := make(map[string]JobState, len(records))
	for _, rec := range records {
		switch rec.Kind {
		case evCompleted:
			terminal[rec.IdemKey] = StateSucceeded
		case evFailed:
			terminal[rec.IdemKey] = StateFailed
		case evCanceled:
			terminal[rec.IdemKey] = StateCanceled
		}
	}

	// 终态记忆要在重建作业之前灌进去：下游作业的依赖判定依赖它，
	// 否则「上游已失败」会被误判成「上游还没出现」而永远等待。
	for k, st := range terminal {
		e.sched.RememberTerminal(k, st)
	}

	// 第一遍：重建租户份额基线。
	//
	// 这里播种的是「崩溃前的历史峰值」，用来给 DRF 一个非零起点，
	// 避免重启瞬间所有租户份额都是 0 而集体抢跑。但它只是打分用的基线，
	// 绝不能进入真正的占用账本 reserved——那些作业马上会被重新派发，
	// 一旦再扣一次就会双倍计费，把池子算成已满、谁也调度不出去。
	for _, rec := range records {
		for tenant, usage := range rec.TenantUsage {
			e.sched.SeedTenantWarm(tenant, usage)
		}
	}

	// 第二遍：重建作业。
	seen := make(map[string]struct{}, len(records))
	recovered := 0
	for _, rec := range records {
		switch rec.Kind {
		case evSubmitted, evDispatched:
			if _, done := terminal[rec.IdemKey]; done {
				// 已经跑完的作业只保留终态记忆（供下游依赖判定用），不重建。
				continue
			}
			if rec.IdemKey == "" {
				continue
			}
			if _, dup := seen[rec.IdemKey]; dup {
				continue // 只入队一次，重放同样遵守幂等
			}
			seen[rec.IdemKey] = struct{}{}
			req := SubmitRequest{
				Tenant:     rec.Tenant,
				IdemKey:    rec.IdemKey,
				Group:      rec.Group,
				Name:       rec.Name,
				Want:       rec.Want,
				DepRefs:    rec.DepRefs,
				MaxRetries: rec.MaxRetries,
				Work:       rec.Work,
				Force:      true,
			}
			job, _, err := e.sched.submitRecovering(req)
			if err != nil {
				if e.log != nil {
					e.log("WARN recovery failed for %s: %v", rec.IdemKey, err)
				}
				continue
			}
			job.Recovered = true
			recovered++
		}
	}
	return recovered
}

// ---------------------------------------------------------------------------
// 默认执行体
// ---------------------------------------------------------------------------

// errScriptedFailure 是 demo 用来触发重试的合成错误。
var errScriptedFailure = errors.New("scripted transient failure")

// errFatalWork 是执行体主动放弃的错误，视为不可重试。
var errFatalWork = errors.New("fatal work error")

// defaultWorkFunc 是内置执行体：按 WorkSpec 逐单元推进，单元之间检查
// ctx，从而保证「取消信号能在有界时间内停下来」。
func defaultWorkFunc(ctx context.Context, job *Job) (Result, error) {
	units := job.Work.Units
	if units <= 0 {
		units = 1
	}
	per := job.Work.UnitDuration
	if per <= 0 {
		per = time.Millisecond
	}

	// 每次尝试开头检查一次：作业可能一启动就已被取消（上游已失败）。
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	items := 0
	for i := 0; i < units; i++ {
		// 分片睡眠，保证取消信号能在 per 以内被响应。
		timer := time.NewTimer(per)
		select {
		case <-ctx.Done():
			timer.Stop()
			// 取消路径：把已经算出来的部分结果就地丢弃，不返回给下游。
			// 这就是「部分结果不得泄漏」。
			return Result{}, ctx.Err()
		case <-timer.C:
		}
		items++
	}

	// 可脚本化的失败：让重试与熔断逻辑在 demo 里可被验证。
	if job.Work.FailFirst > 0 && job.Attempts() <= job.Work.FailFirst {
		job.setLastError(fmt.Errorf("%w (attempt %d)", errScriptedFailure, job.Attempts()))
		return Result{JobKey: job.IdemKey, Items: items}, errScriptedFailure
	}
	if job.Work.FailAlways {
		job.setLastError(fmt.Errorf("%w (attempt %d)", errFatalWork, job.Attempts()))
		return Result{JobKey: job.IdemKey, Items: items}, errFatalWork
	}

	// 成功：推进状态机终态。
	if err := job.transition(StateSucceeded, "work completed"); err != nil {
		return Result{JobKey: job.IdemKey, Items: items}, err
	}
	return Result{JobKey: job.IdemKey, Items: items,
		Meta: map[string]string{"attempts": fmt.Sprint(job.Attempts())}}, nil
}

// ---------------------------------------------------------------------------
// 主循环
// ---------------------------------------------------------------------------

// Start 启动主循环，并在启动瞬间就装好 SIGTERM/SIGINT 处理器。
//
// 信号注册必须放在 Start 里而不是 RunUntilSignal 里：demo/测试会在 Start
// 之后立刻给自己发 SIGTERM，如果处理器还没装上，Go 的默认行为会直接终止
// 进程，优雅停机逻辑一行都跑不到（这个 bug 让 demo 以 exit code 143 结束）。
func (e *Engine) Start() {
	e.start(true)
}

// start 启动引擎。withSignal=false 时不注册进程级信号处理器，
// 供「同一进程里第二个及以后的引擎」使用——signal.Notify 是进程级的，
// 多个引擎各自注册会互相抢信号，导致先启动的那个永远收不到 SIGTERM。
func (e *Engine) start(withSignal bool) {
	if withSignal {
		if e.signalCh == nil {
			e.signalCh = make(chan os.Signal, 1)
			signal.Notify(e.signalCh, syscall.SIGTERM, syscall.SIGINT)
		}
		go e.signalWatcher()
	}
	go e.loop()
	go e.reaper()
	go e.drainer()
}

// signalWatcher 把捕获到的信号转交给本引擎。
//
// 注意：demo 会在同一个进程里先后创建多个引擎，而 signal.Notify 是进程级的。
// 如果每个引擎都自己 Notify，那么后创建的引擎会把前一个的信号「抢走」，
// 导致先创建的引擎永远收不到 SIGTERM。这里用 sync.Once 让整进程只注册一次，
// 再由当前活跃引擎（signalTarget）接收，保证信号一定送达正确的那一个。
func (e *Engine) signalWatcher() {
	sig, ok := <-e.signalCh
	if !ok {
		return
	}
	e.logf("收到信号 %s，开始优雅停机", sig)
	e.beginStop()
}

// StartNoSignal 启动引擎但不接管进程信号。
// 用于同一进程内后续创建的引擎（例如 demo 的 WAL 重放阶段）。
func (e *Engine) StartNoSignal() { e.start(false) }

// WaitSignal 阻塞直到收到信号（serve 用）。
func (e *Engine) WaitSignal() os.Signal {
	return <-e.signalCh
}

// beginStop 关闭 stopCh，触发 drainer。幂等。
func (e *Engine) beginStop() {
	e.once.Do(func() { close(e.stopCh) })
}

// loop 是决策主循环：串行处理提交、推进依赖、跑 DRF 决策。
func (e *Engine) loop() {
	defer close(e.doneCh)
	ticker := time.NewTicker(e.tick)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case req := <-e.submitCh:
			e.handleSubmit(req)
		case <-ticker.C:
			e.step(time.Now())
		}
	}
}

func (e *Engine) handleSubmit(req submitReq) {
	// 停机后拒绝新提交，但仍然回复，避免调用方永久阻塞。
	if e.sched.Draining() {
		req.reply <- submitReply{err: ErrDraining}
		return
	}
	// 提交阶段就被判定取消的作业（上游已死）也要落 WAL，
	// 否则这条取消决策在日志里查无实据，事后无法审计。
	job, created, err := e.sched.submitWithCancels(req.req, func(j *Job, why string) {
		e.logf("CANCEL job=%s tenant=%s group=%s reason=%q（提交时上游已失败）",
			j.IdemKey, j.Tenant, orDash(j.Group), why)
		_ = e.wal.Append(walRecord{
			Kind: evCanceled, IdemKey: j.IdemKey, Tenant: j.Tenant,
			Group: j.Group, Name: j.Name, Want: j.Want, Err: why,
		}, true)
	})
	if err != nil {
		req.reply <- submitReply{err: err}
		return
	}
	if !created {
		// 幂等命中：返回已存在句柄，不重复入队。
		req.reply <- submitReply{job: job, created: false}
		return
	}
	// WAL 追加：先落盘再让它进入调度视野，保证崩溃不丢在途作业。
	if err := e.wal.Append(e.submittedRecord(job), true); err != nil {
		e.logf("ERROR wal append on submit %s: %v", job.IdemKey, err)
	}
	req.reply <- submitReply{job: job, created: true}
}

func (e *Engine) submittedRecord(job *Job) walRecord {
	return walRecord{
		Kind:       evSubmitted,
		IdemKey:    job.IdemKey,
		Tenant:     job.Tenant,
		Group:      job.Group,
		Name:       job.Name,
		Want:       job.Want,
		DepRefs:    job.DepRefs,
		MaxRetries: job.MaxRetries,
		Work:       job.Work,
		Recovered:  job.Recovered,
	}
}

// step 跑一轮：解决依赖 → DRF 决策 → 派发执行。
func (e *Engine) step(now time.Time) {
	if e.sched.Draining() {
		return
	}
	// 1) 依赖就绪/失效判定。上游失败的连带取消在这里落 WAL，
	//    记录必须带 group，否则事后无法把事件归到具体的依赖组上。
	for _, job := range e.sched.ResolveDependencies() {
		if job.State() != StateCanceled {
			continue
		}
		e.logf("CANCEL job=%s tenant=%s group=%s reason=%q（上游失败，依赖链连带取消）",
			job.IdemKey, job.Tenant, orDash(job.Group), errString(job.LastError()))
		_ = e.wal.Append(walRecord{
			Kind:    evCanceled,
			IdemKey: job.IdemKey,
			Tenant:  job.Tenant,
			Group:   job.Group,
			Name:    job.Name,
			Want:    job.Want,
			Err:     errString(job.LastError()),
		}, true)
	}
	// 2) DRF 决策（Plan）与资源扣减（Acquire）分离：Plan 只读调度器状态，
	//    Acquire 在调度器锁外扣资源，避免与释放路径形成锁序反转。
	for i := 0; i < 64; i++ { // 单轮最多连续派发 64 个，避免饿死其他租户
		p, se := e.sched.Plan(e.pool, now)
		if p == nil {
			e.noteSelectMiss(se)
			return
		}
		// 3) 扣减资源。扣不动就把作业原样留在队列里，下一轮再试。
		if e.sched.Acquire(e.pool, p) == nil {
			e.sched.RequeueAfterFail(p.Job)
			return
		}
		job := p.Job
		e.logf("DISPATCH job=%s tenant=%-6s group=%-10s want=%s score=%.4f share %.4f->%.4f (%s)",
			job.IdemKey, job.Tenant, orDash(job.Group), job.Want,
			p.Score, p.Before, p.After, job.Name)
		// WAL：调度决策必须先落盘（预写），再真正启动执行体。
		if err := e.wal.Append(walRecord{
			Kind:        evDispatched,
			IdemKey:     job.IdemKey,
			Tenant:      job.Tenant,
			Group:       job.Group,
			Want:        job.Want,
			Attempts:    job.Attempts(),
			TenantUsage: map[string]Resources{job.Tenant: e.sched.TenantReserved(job.Tenant)},
		}, true); err != nil {
			e.logf("ERROR wal append on dispatch %s: %v", job.IdemKey, err)
		}
		// 4) 派发到 errgroup 执行组。
		e.runner.Dispatch(e.rootCtx, job, e.runFunc)
	}
}

func (e *Engine) noteSelectMiss(se selectErrors) {
	if se.Drained {
		return
	}
	if len(se.Breakers) > 0 && !se.NoFit {
		e.logf("BLOCK breaker %s", strings.Join(se.Breakers, "; "))
	}
}

// ---------------------------------------------------------------------------
// 回收：把完成的组结算掉
// ---------------------------------------------------------------------------

// reaper 定期检查已完成的作业组并结算终态。
func (e *Engine) reaper() {
	t := time.NewTicker(e.tick)
	defer t.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-t.C:
			// 结算判据是「组内有已跑完待结算的成员」，而不是任何计数器：
			// 计数器会在依赖组分批派发时给出错误信号，导致下游成员永远无人结算。
			for _, name := range e.runner.GroupNames() {
				grp, ok := e.runner.Group(name)
				if !ok || !grp.hasPendingSettle() {
					continue
				}
				e.runner.Settle(grp)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 优雅停机
// ---------------------------------------------------------------------------

// drainer 是排空阶段的状态机。
//
// 四个阶段的顺序对应需求里的优雅停机四步：
//  1. BeginDrain：停止接收新提交与新调度决策；
//  2. DrainAll(30s 预算)：给在途作业自然跑完的机会，超时才强制取消残留；
//  3. CancelPending：把还没开始的作业标记取消，避免它们永远悬在队列里；
//  4. 最终 fsync + Close：确保停机点之后的重放是完整的。
func (e *Engine) drainer() {
	defer close(e.drainDone)

	// 必须先等停机信号再开始排空。
	// 少了这一行，drainer 会在 Start() 的瞬间就进入排空流程，
	// 主循环还没跑过一个 tick 就被 BeginDrain 掉，随后所有提交都会收到
	// "scheduler is draining"——表现就是 demo 在场景 2 立刻失败。
	<-e.stopCh

	e.logf("=== 优雅停机开始 (drain_budget=%s) ===", e.drainBudget)
	start := time.Now()

	// 阶段 1：停止接收新提交与新调度。
	e.sched.BeginDrain()

	// 阶段 2：在预算内给在途作业自然排空的机会。这里不主动 cancel 根
	// context —— 让作业跑完；只有 WaitTimeout 超时才会取消残留任务。
	clean, detail := e.runner.DrainAll(e.drainBudget)

	// 阶段 3：清理尚未开始的作业（它们永远不会在本次运行中执行）。
	canceled := e.sched.CancelPending("scheduler draining at shutdown")
	for _, job := range canceled {
		_ = e.wal.Append(walRecord{
			Kind: evCanceled, IdemKey: job.IdemKey, Tenant: job.Tenant, Group: job.Group,
			Name: job.Name, Want: job.Want,
			Err: "canceled during shutdown (never started)",
		}, false)
	}
	if len(canceled) > 0 {
		e.logf("DRAIN canceled %d never-started job(s)", len(canceled))
	}

	// 阶段 4：最后一次 fsync，确保停机点之后的重放是完整的。
	_ = e.wal.Append(walRecord{
		Kind: evShutdown,
		Err: fmt.Sprintf("clean=%v in_flight=%d elapsed=%s",
			clean, e.runner.InFlight(), time.Since(start).Round(time.Millisecond)),
	}, true)
	if err := e.wal.Flush(); err != nil {
		e.logf("ERROR final wal flush: %v", err)
	}
	if err := e.wal.Close(); err != nil {
		e.logf("ERROR wal close: %v", err)
	}

	e.drainClean, e.drainDetail = clean, detail
	e.logf("=== 优雅停机完成 clean=%v detail=%s elapsed=%s ===",
		clean, detail, time.Since(start).Round(time.Millisecond))
	e.rootCancel()
}

// Shutdown 是对外的停机入口。幂等。返回排空是否干净。
func (e *Engine) Shutdown(ctx context.Context) (clean bool, detail string) {
	// beginStop 幂等：无论是信号触发的还是 API 调用触发的，排空只跑一次。
	e.beginStop()

	// 等主循环退出（停止接收新提交）。
	select {
	case <-e.doneCh:
	case <-ctx.Done():
		return false, "shutdown timed out waiting for main loop"
	}
	// 再等排空与最终落盘完成。drainer 是唯一写 drainClean/drainDetail 的地方，
	// 必须等它关闭 drainDone，否则读到的会是零值。
	select {
	case <-e.drainDone:
	case <-ctx.Done():
		return false, "shutdown timed out waiting for drain"
	}
	return e.drainClean, e.drainDetail
}

// RunUntilSignal 启动引擎并阻塞直到收到 SIGTERM/SIGINT，然后优雅停机。
// serve 子命令走这条路径。
func (e *Engine) RunUntilSignal() (clean bool, detail string) {
	e.Start()
	e.WaitSignal()
	ctx, cancel := context.WithTimeout(context.Background(), e.drainBudget+10*time.Second)
	defer cancel()
	return e.Shutdown(ctx)
}

// selfSignal 给自己的进程发 SIGTERM。这是验证优雅停机最真实的做法：
// 走的是与现网完全相同的 os/signal 路径，而不是直接调 Shutdown。
func selfSignal() error {
	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

// ---------------------------------------------------------------------------
// CLI 基础工具
// ---------------------------------------------------------------------------

func usage() {
	fmt.Fprintf(os.Stderr, `aisched — DRF 公平共享的共享算力池调度器

用法:
  aisched submit   提交作业（幂等键去重，重复提交返回同一句柄）
  aisched serve    启动调度器并等待 SIGTERM（优雅停机 + WAL 恢复）
  aisched drain    排空并打印最终状态
  aisched status   打印资源池 / 租户份额 / 熔断器 / WAL 统计
  aisched demo     跑内置示例并验证优雅停机与 WAL 重放恢复
  aisched bench    跑性能自检（DRF 决策 / 资源池 / WAL fsync / 停机）

池容量默认 64 核 / 256GB 内存 / 8 张 GPU，与需求中的共享算力池一致。
示例:
  aisched submit -tenant cv -key train-1 -res cpu=32,mem=98304,gpu=4 -units 5
  aisched submit -tenant data -key prep-1 -dep train-1 -res cpu=8,mem=16384
  aisched serve -wal ./aisched.wal -v
`)
}

// main 是终端入口：分派子命令，并把信号处理交给各 Engine 自行注册。
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, rest := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "submit":
		err = cmdSubmit(rest)
	case "serve":
		err = cmdServe(rest)
	case "drain":
		err = cmdDrain(rest)
	case "status":
		err = cmdStatus(rest)
	case "demo":
		err = cmdDemo(rest)
	case "bench":
		err = cmdBench(rest)
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

// orDash 把空串显示成 "-"。

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// errString 安全地把 error 转成字符串。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func defaultTotal() Resources { return Resources{CPU: 64, MemMB: 256 * 1024, GPU: 8} }

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

// defaultWALPath 给出默认 WAL 位置，优先放用户缓存目录。
func defaultWALPath() string {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "aisched", "aisched.wal")
}

// newLogger 构造带相对时间戳的日志出口；verbose=false 时返回 nil（静音）。
func newLogger(verbose bool) Logger {
	if !verbose {
		return nil
	}
	start := time.Now()
	return func(format string, args ...any) {
		fmt.Printf("[%7s] %s\n", time.Since(start).Round(time.Millisecond), fmt.Sprintf(format, args...))
	}
}

// jsonIndent 是 json.MarshalIndent 的薄封装。
func jsonIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func cmdServe(args []string) error {
	fs := newFlagSet("serve")
	fs.String("wal", defaultWALPath(), "WAL 文件路径")
	verbose := fs.Bool("v", true, "打印调度轨迹")
	poolCPU := fs.Int64("cpu", defaultTotal().CPU, "池 CPU 核数")
	poolMem := fs.Int64("mem", defaultTotal().MemMB, "池内存 MB")
	poolGPU := fs.Int64("gpu", defaultTotal().GPU, "池 GPU 卡数")
	drain := fs.Duration("drain-budget", 30*time.Second, "优雅停机排空预算")
	if err := fs.Parse(args); err != nil {
		return err
	}
	eng, err := NewEngine(EngineOptions{
		WALPath:     fs.Lookup("wal").Value.String(),
		PoolTotal:   Resources{CPU: *poolCPU, MemMB: *poolMem, GPU: *poolGPU},
		Logger:      newLogger(*verbose),
		DrainBudget: *drain,
	})
	if err != nil {
		return err
	}
	clean, detail := eng.RunUntilSignal()
	fmt.Printf("停机结果 clean=%v detail=%s\n", clean, detail)
	return nil
}

// ---------------------------------------------------------------------------
// drain / status
// ---------------------------------------------------------------------------

func cmdDrain(args []string) error {
	fs := newFlagSet("drain")
	walPath := fs.String("wal", defaultWALPath(), "WAL 文件路径")
	verbose := fs.Bool("v", true, "打印调度轨迹")
	poolCPU := fs.Int64("cpu", defaultTotal().CPU, "池 CPU 核数")
	poolMem := fs.Int64("mem", defaultTotal().MemMB, "池内存 MB")
	poolGPU := fs.Int64("gpu", defaultTotal().GPU, "池 GPU 卡数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	eng, err := NewEngine(EngineOptions{
		WALPath:   *walPath,
		PoolTotal: Resources{CPU: *poolCPU, MemMB: *poolMem, GPU: *poolGPU},
		Logger:    newLogger(*verbose),
	})
	if err != nil {
		return err
	}
	clean, detail := eng.Shutdown(context.Background())
	printStatus(eng, false)
	fmt.Printf("排空结果 clean=%v detail=%s\n", clean, detail)
	return nil
}

func cmdStatus(args []string) error {
	fs := newFlagSet("status")
	walPath := fs.String("wal", defaultWALPath(), "WAL 文件路径")
	jsonOut := fs.Bool("json", false, "JSON 输出")
	poolCPU := fs.Int64("cpu", defaultTotal().CPU, "池 CPU 核数")
	poolMem := fs.Int64("mem", defaultTotal().MemMB, "池内存 MB")
	poolGPU := fs.Int64("gpu", defaultTotal().GPU, "池 GPU 卡数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// status 会打开 WAL 并重放在途作业，因此必须给出与 serve 一致的池容量，
	// 否则 DRF 份额没有量纲、资源池也无法构造。
	eng, err := NewEngine(EngineOptions{
		WALPath:   *walPath,
		PoolTotal: Resources{CPU: *poolCPU, MemMB: *poolMem, GPU: *poolGPU},
	})
	if err != nil {
		return err
	}
	printStatus(eng, *jsonOut)
	return nil
}

func printStatus(e *Engine, jsonOut bool) {
	snap := e.pool.Snapshot()
	shares := e.sched.Shares(e.pool)
	if jsonOut {
		b, _ := jsonMarshalIndent(struct {
			Pool   PoolSnapshot   `json:"pool"`
			Shares []tenantShare  `json:"shares"`
			Sched  SchedulerStats `json:"scheduler"`
			Runner RunnerStats    `json:"runner"`
			WAL    WALStats       `json:"wal"`
		}{snap, shares, e.sched.Stats(), e.runner.Stats(), e.wal.Stats()})
		fmt.Println(string(b))
		return
	}
	fmt.Printf("资源池: %s\n", snap)
	fmt.Printf("可用:   %s\n", e.pool.Available())
	fmt.Printf("\n租户 DRF 主导份额:\n")
	fmt.Printf("%-12s %-22s %8s %7s %7s %7s\n", "TENANT", "USAGE", "SHARE", "PEND", "RUN", "COLD")
	for _, sh := range shares {
		cold := ""
		if sh.ColdStart {
			cold = "yes"
		}
		fmt.Printf("%-12s %-22s %8.4f %7d %7d %7s\n", sh.Tenant, sh.Usage.String(), sh.Share, sh.Pending, sh.Running, cold)
	}
	ss := e.sched.Stats()
	fmt.Printf("\n调度器: decisions=%d rejected_size=%d rejected_breaker=%d ready=%d waiting=%d active=%d\n",
		ss.Decisions, ss.RejectedSize, ss.RejectedBreak, ss.Ready, ss.Waiting, ss.Active)
	rs := e.runner.Stats()
	fmt.Printf("执行器: executions=%d ok=%d fail=%d canceled=%d retries=%d double_run_skipped=%d in_flight=%d\n",
		rs.Executions, rs.Successes, rs.Failures, rs.Canceled, rs.Retries, rs.DoubleRunSkip, rs.InFlight)
	ws := e.wal.Stats()
	fmt.Printf("WAL:    %s\n", ws)
	fmt.Printf("熔断器阈值: 连续失败 %d 次熔断 %s，半开放 %d 个探测作业\n",
		breakerFailureThreshold, breakerCooldown, breakerHalfOpenProbes)
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func jsonMarshalIndent(v any) ([]byte, error) {
	return jsonIndent(v)
}

// parseResource 解析 "cpu=8,mem=32768,gpu=2" 这样的规格串。
func parseResource(spec string) (Resources, error) {
	var r Resources
	if strings.TrimSpace(spec) == "" {
		return r, errors.New("empty resource spec")
	}
	for _, part := range strings.Split(spec, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return r, fmt.Errorf("bad resource pair %q, want key=value", part)
		}
		var dst *int64
		switch strings.TrimSpace(kv[0]) {
		case "cpu", "c":
			dst = &r.CPU
		case "mem", "memory", "m":
			dst = &r.MemMB
		case "gpu", "g":
			dst = &r.GPU
		default:
			return r, fmt.Errorf("unknown resource key %q", kv[0])
		}
		n, err := parseInt64(kv[1])
		if err != nil {
			return r, fmt.Errorf("parse %s: %w", kv[0], err)
		}
		*dst = n
	}
	return r, nil
}

// parseInt64 是不依赖 strconv 的十进制解析（保持零第三方依赖）。
func parseInt64(str string) (int64, error) {
	str = strings.TrimSpace(str)
	if str == "" {
		return 0, errors.New("empty number")
	}
	var n int64
	neg := false
	for i := 0; i < len(str); i++ {
		c := str[i]
		if i == 0 && (c == '-' || c == '+') {
			neg = c == '-'
			continue
		}
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid digit %q in %q", c, str)
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

// parseDuration 解析时长，空串返回 0。
func parseDuration(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// ---------------------------------------------------------------------------
// submit
// ---------------------------------------------------------------------------

// cmdSubmit 提交一个作业。
//
// 它会故意立刻用同一个幂等键再提交一次，把「手抖重复提交」的结果直接
// 打印出来：第二次必须返回同一个句柄（created=false），而不是新建一份。
func cmdSubmit(args []string) error {
	fs := newFlagSet("submit")
	walPath := fs.String("wal", defaultWALPath(), "WAL 文件路径")
	verbose := fs.Bool("v", false, "打印调度轨迹")
	jsonOut := fs.Bool("json", false, "以 JSON 输出")

	tenant := fs.String("tenant", "", "租户名（必填）")
	idem := fs.String("key", "", "幂等键（必填，同键重复提交只入队一次）")
	group := fs.String("group", "", "作业组 ID，同组共享 errgroup 取消传播")
	name := fs.String("name", "", "作业名")
	res := fs.String("res", "", "资源需求，如 cpu=8,mem=32768,gpu=2")
	depList := fs.String("dep", "", "上游幂等键，逗号分隔")
	maxRetries := fs.Int("retries", 3, "最大重试次数")
	units := fs.Int("units", 1, "计算单元数")
	unitDur := fs.String("unit", "50ms", "每单元耗时")
	failFirst := fs.Int("fail-first", 0, "前 N 次尝试失败（测试用）")
	runFor := fs.String("run", "3s", "执行多久后向自身发送 SIGTERM")
	poolCPU := fs.Int64("cpu", defaultTotal().CPU, "池 CPU 核数")
	poolMem := fs.Int64("mem", defaultTotal().MemMB, "池内存 MB")
	poolGPU := fs.Int64("gpu", defaultTotal().GPU, "池 GPU 卡数")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenant == "" || *idem == "" {
		return errors.New("-tenant 与 -key 均为必填")
	}
	want, err := parseResource(*res)
	if err != nil {
		return err
	}
	per, err := parseDuration(*unitDur)
	if err != nil {
		return err
	}
	runDur, err := parseDuration(*runFor)
	if err != nil {
		return err
	}

	eng, err := NewEngine(EngineOptions{
		WALPath:   *walPath,
		PoolTotal: Resources{CPU: *poolCPU, MemMB: *poolMem, GPU: *poolGPU},
		Logger:    newLogger(*verbose),
		WorkFunc:  defaultWorkFunc,
	})
	if err != nil {
		return err
	}
	eng.Start()

	var deps Dependencies
	if *depList != "" {
		deps = strings.Split(*depList, ",")
	}
	ctx := context.Background()
	req := SubmitRequest{
		Tenant:     *tenant,
		IdemKey:    *idem,
		Group:      *group,
		Name:       *name,
		Want:       want,
		Deps:       deps,
		MaxRetries: *maxRetries,
		Work:       WorkSpec{Units: *units, UnitDuration: per, FailFirst: *failFirst},
	}
	job, created, err := eng.Submit(ctx, req)
	if err != nil {
		return err
	}
	// 立刻用同一个 key 再提交一次，验证幂等。
	dup, dupCreated, dupErr := eng.Submit(ctx, req)

	report := struct {
		Key        string `json:"key"`
		Created    bool   `json:"created"`
		Duplicate  bool   `json:"duplicate_detected"`
		SameHandle bool   `json:"same_handle"`
		OwnerName  string `json:"name"`
		State      string `json:"state"`
	}{
		Key: job.IdemKey, Created: created, OwnerName: job.Name, State: string(job.State()),
	}
	if dupErr == nil {
		report.Duplicate = !dupCreated
		report.SameHandle = dup == job
	}
	if *jsonOut {
		b, _ := jsonIndent(report)
		fmt.Println(string(b))
	} else {
		fmt.Printf("已提交 key=%s created=%v state=%s name=%q\n",
			report.Key, report.Created, report.State, report.OwnerName)
		if report.Duplicate {
			fmt.Printf("重复提交被识别 same_handle=%v —— 未二次入队，GPU 不会被双份占用\n", report.SameHandle)
		}
	}

	fmt.Printf("运行 %s 后向自身发送 SIGTERM（真实信号）...\n", runDur)
	time.Sleep(runDur)
	if err := selfSignal(); err != nil {
		return err
	}
	clean, detail := eng.Shutdown(context.Background())
	fmt.Printf("停机结果 clean=%v detail=%s\n", clean, detail)
	fmt.Printf("最终资源池: %s\n", eng.pool.Snapshot())
	return nil
}

// ---------------------------------------------------------------------------
// bench
// ---------------------------------------------------------------------------

// cmdBench 跑性能自检，对齐需求里的四条硬指标。
func cmdBench(args []string) error {
	fs := newFlagSet("bench")
	walPath := fs.String("wal", filepath.Join(os.TempDir(), "aisched-bench.wal"), "WAL 文件路径（会被复用）")
	pending := fs.Int("pending", 1000, "DRF 决策测试用的待调度作业数")
	conc := fs.Int("conc", 100, "并发作业数")
	iters := fs.Int("iters", 200, "资源池扣减迭代次数")
	if err := fs.Parse(args); err != nil {
		return err
	}

	total := defaultTotal()
	results := make([]checkResult, 0, 6)

	// --- 1) DRF 决策 < 1ms @ 1000 待调度作业 ---
	pool, _ := NewPool(total)
	sched := NewScheduler(schedulerOptions{})
	tenants := []string{"cv", "nlp", "rec", "eval", "data"}
	for i := 0; i < *pending; i++ {
		t := tenants[i%len(tenants)]
		want := Resources{
			CPU:   int64(1 + (i*7)%16),
			MemMB: int64(512 * (1 + (i*3)%8)),
			GPU:   int64((i * 5) % 4),
		}
		_, _, _ = sched.Submit(SubmitRequest{
			Tenant: t, IdemKey: fmt.Sprintf("bench-%d", i), Want: want,
			MaxRetries: 0, Work: WorkSpec{Units: 1},
		})
	}
	drfDur := measure(func() {
		// 决策失败也算一次完整决策路径（选不中同样是有效决策）。
		p, _ := sched.Plan(pool, time.Now())
		if p != nil {
			_ = sched.Acquire(pool, p)
		}
	}, 200)
	schedStats := sched.Stats()
	results = append(results, checkResult{
		Name:   "DRF 单次决策 (1000 待调度)",
		Value:  drfDur,
		Budget: 1 * time.Millisecond,
		Note:   fmt.Sprintf("decisions=%d rejected_size=%d", schedStats.Decisions, schedStats.RejectedSize),
	})

	// --- 2) 资源池扣减 < 100µs 高锁竞争 ---
	cpool, _ := NewPool(total)
	cpu := resList(64, tenants, *iters)
	const workers = 8
	// 每个 goroutine 跑 iters 次「扣减 + 归还」，共 workers*iters 对操作。
	var ops int64
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ { // 8 个 goroutine 共抢同一把锁
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < *iters; i++ {
				n := w**iters + i
				k := fmt.Sprintf("bench-acq-%d", n%64)
				tn := tenants[n%len(tenants)]
				_ = cpool.Acquire(tn, k, cpu[n%len(cpu)])
				cpool.Release(tn, k, cpu[n%len(cpu)])
			}
			atomic.AddInt64(&ops, int64(*iters))
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	perOp := elapsed / time.Duration(ops)
	ps := cpool.Stats()
	results = append(results, checkResult{
		Name:   "资源池 Acquire+Release (8 goroutine 竞争)",
		Value:  perOp,
		Budget: 100 * time.Microsecond,
		Note:   fmt.Sprintf("calls=%d max_lock=%s", ps.AcquireCalls, time.Duration(ps.MaxLockNanos)),
	})

	// --- 3) WAL fsync < 5ms ---
	_ = os.Remove(*walPath)
	w, _, err := OpenWAL(*walPath)
	if err != nil {
		return err
	}
	syncDur := measure(func() {
		_ = w.Append(walRecord{Kind: evSubmitted, IdemKey: "bench", Tenant: "bench",
			Want: Resources{CPU: 1, MemMB: 1, GPU: 0}}, true)
	}, *iters)
	w.Close()
	results = append(results, checkResult{
		Name:   "WAL 单次 fsync",
		Value:  syncDur,
		Budget: 5 * time.Millisecond,
		Note:   w.Stats().String(),
	})

	// --- 4) WAL 组提交（并发追加时的 fsync 摊薄） ---
	_ = os.Remove(*walPath)
	gw, _, err := openWAL(*walPath, walOptions{syncMode: true, groupWindow: 2 * time.Millisecond})
	if err != nil {
		return err
	}
	groupDur := measureParallel(16, 64, func(i int) {
		_ = gw.Append(walRecord{Kind: evSubmitted, IdemKey: fmt.Sprintf("g-%d", i), Tenant: "bench",
			Want: Resources{CPU: 1}}, true)
	})
	gws := gw.Stats()
	gw.Close()
	results = append(results, checkResult{
		Name:   "WAL 并发追加 fsync (组提交)",
		Value:  groupDur,
		Budget: 5 * time.Millisecond,
		Note: fmt.Sprintf("appends=%d fsyncs=%d 摊薄比=%.1fx", gws.Appends, gws.Fsyncs,
			float64(gws.Appends)/float64(max64(gws.Fsyncs, 1))),
	})

	// --- 5) 100 并发作业端到端 ---
	e2, err := NewEngine(EngineOptions{
		WALPath:   filepath.Join(os.TempDir(), "aisched-bench-e2e.wal"),
		PoolTotal: total,
		Tick:      2 * time.Millisecond,
		Logger:    nil,
		WorkFunc: func(ctx context.Context, job *Job) (Result, error) {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(time.Millisecond):
			}
			_ = job.transition(StateSucceeded, "bench")
			return Result{JobKey: job.IdemKey, Items: 1}, nil
		},
	})
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(os.TempDir(), "aisched-bench-e2e.wal"))
	e2.Start()
	var swg sync.WaitGroup
	for i := 0; i < *conc; i++ {
		swg.Add(1)
		go func(i int) {
			defer swg.Done()
			_, _, _ = e2.Submit(context.Background(), SubmitRequest{
				Tenant:  tenants[i%len(tenants)],
				IdemKey: fmt.Sprintf("e2e-%d", i),
				Want:    Resources{CPU: 1, MemMB: 64, GPU: 0},
				Work:    WorkSpec{Units: 1},
			})
		}(i)
	}
	swg.Wait()
	e2Start := time.Now()
	deadline := e2Start.Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if e2.sched.PendingCount() == 0 && e2.runner.InFlight() == 0 &&
			e2.pool.Allocated() == (Resources{}) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	e2Elapsed := time.Since(e2Start)
	_, _ = e2.Shutdown(context.Background())
	e2st := e2.runner.Stats()
	results = append(results, checkResult{
		Name:   fmt.Sprintf("%d 并发作业端到端跑完", *conc),
		Value:  e2Elapsed,
		Budget: 20 * time.Second,
		Note: fmt.Sprintf("ok=%d fail=%d elapsed=%s（%.0f 作业/秒）",
			e2st.Successes, e2st.Failures, e2Elapsed.Round(time.Millisecond),
			float64(e2st.Successes)/e2Elapsed.Seconds()),
	})

	// --- 6) 优雅停机 < 30s ---
	e3, err := NewEngine(EngineOptions{
		WALPath:     filepath.Join(os.TempDir(), "aisched-bench-drain.wal"),
		PoolTotal:   total,
		Tick:        2 * time.Millisecond,
		DrainBudget: 30 * time.Second,
		WorkFunc: func(ctx context.Context, job *Job) (Result, error) {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
			_ = job.transition(StateSucceeded, "bench")
			return Result{JobKey: job.IdemKey, Items: 1}, nil
		},
	})
	if err != nil {
		return err
	}
	e3.Start()
	for i := 0; i < 20; i++ {
		_, _, _ = e3.Submit(context.Background(), SubmitRequest{
			Tenant: "drain", IdemKey: fmt.Sprintf("drain-%d", i),
			Want: Resources{CPU: 1, MemMB: 128, GPU: 0},
			Work: WorkSpec{Units: 1},
		})
	}
	time.Sleep(120 * time.Millisecond) // 让一些作业进入 running
	drainStart := time.Now()
	clean, detail := e3.Shutdown(context.Background())
	drainDur := time.Since(drainStart)
	results = append(results, checkResult{
		Name:   "优雅停机排空+落盘",
		Value:  drainDur,
		Budget: 30 * time.Second,
		Note:   fmt.Sprintf("clean=%v detail=%s", clean, detail),
	})

	// --- 汇总 ---
	fmt.Println()
	allPass := true
	for _, r := range results {
		status := "PASS"
		if r.Value > r.Budget {
			status = "FAIL"
			allPass = false
		}
		fmt.Printf("[%s] %-40s %14s  (预算 %s)\n", status, r.Name, formatLatency(r.Value), formatLatency(r.Budget))
		if r.Note != "" {
			fmt.Printf("       %s\n", r.Note)
		}
	}
	fmt.Println()
	if allPass {
		fmt.Println("全部性能指标通过。")
	} else {
		fmt.Println("存在超预算项。")
	}
	return nil
}

// formatLatency 用自适应单位呈现耗时：亚微秒级不能直接四舍五入成 0s，
// 否则「远低于预算」会被误读成「没测出来」。
func formatLatency(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.2fµs", float64(d.Nanoseconds())/1e3)
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d.Nanoseconds())/1e6)
	default:
		return d.Round(time.Millisecond).String()
	}
}

type checkResult struct {
	Name   string
	Value  time.Duration
	Budget time.Duration
	Note   string
}

// measure 跑 n 轮闭包，返回平均耗时。
func measure(fn func(), n int) time.Duration {
	fn() // 预热一次，排除首次分页/栈扩容噪声
	start := time.Now()
	for i := 0; i < n; i++ {
		fn()
	}
	return time.Since(start) / time.Duration(n)
}

// measureParallel 用 w 个 goroutine 各跑 n 次，返回单次平均耗时。
func measureParallel(w, n int, fn func(i int)) time.Duration {
	start := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < w; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				fn(g*n + i)
			}
		}(g)
	}
	wg.Wait()
	return time.Since(start) / time.Duration(w*n)
}

func resList(n int, tenants []string, iters int) []Resources {
	out := make([]Resources, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Resources{CPU: 1, MemMB: 128, GPU: 0})
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// demo
// ---------------------------------------------------------------------------

// cmdDemo 跑一组内置示例，覆盖需求点名的每一个行为：
// 依赖组取消传播、重复提交去重、触发熔断与半开放探活、优雅停机、WAL 重放恢复。
//
// 它会用真实 SIGTERM 打到自身进程（syscall.Kill(getpid(), SIGTERM)），
// 走的是和现网完全一样的信号处理路径，不是模拟。
func cmdDemo(args []string) error {
	fs := newFlagSet("demo")
	walPath := fs.String("wal", filepath.Join(os.TempDir(), "aisched-demo.wal"), "WAL 文件路径")
	drain := fs.Duration("drain-budget", 30*time.Second, "优雅停机排空预算")
	keep := fs.Bool("keep-wal", false, "保留 demo 的 WAL（用于后续 status）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// 每次 demo 从干净日志开始，保证输出可复现。
	if !*keep {
		_ = os.Remove(*walPath)
	}

	logf := func(format string, args ...any) {
		fmt.Printf("  "+format+"\n", args...)
	}

	section := func(title string) {
		fmt.Printf("\n\033[1m=== %s ===\033[0m\n", title)
	}

	// 用缩短的熔断冷却时间，让 60 秒熔断在 demo 时长内可见，
	// 但阈值与半开放语义完全一致。
	schedOpts := defaultSchedulerOptions()
	schedOpts.breakerCooldown = 2 * time.Second
	// 老化量子缩短，让饥饿补偿在 demo 的几秒内可观察。
	schedOpts.waitQuantum = 400 * time.Millisecond

	eng, err := NewEngine(EngineOptions{
		WALPath:     *walPath,
		PoolTotal:   Resources{CPU: 64, MemMB: 256 * 1024, GPU: 8},
		Sched:       schedOpts,
		Tick:        3 * time.Millisecond,
		DrainBudget: *drain,
		Logger:      logf,
		WorkFunc:    demoWorkFunc,
	})
	if err != nil {
		return err
	}
	eng.runner.SetBackoff(80*time.Millisecond, 400*time.Millisecond)
	eng.Start()
	ctx := context.Background()

	// -----------------------------------------------------------------
	section("场景 1：共享池与四个团队（64 核 / 256GB / 8 卡）")
	fmt.Printf("  资源池: %s\n", eng.pool.Total())
	fmt.Printf("  初始可用: %s\n", eng.pool.Available())

	// -----------------------------------------------------------------
	section("场景 2：DRF 公平共享 —— CV 的大训练不再独占")
	// CV 反复提交大训练（8 卡 / 32 核），其余三家提交轻量评测与预处理。
	type sub struct {
		tenant string
		key    string
		name   string
		want   Resources
		work   WorkSpec
	}
	var batch []sub
	for round := 0; round < 3; round++ {
		for i := 0; i < 2; i++ {
			batch = append(batch, sub{"cv", fmt.Sprintf("cv-train-%d-%d", round, i), "cv-resnet-train",
				Resources{CPU: 32, MemMB: 96 * 1024, GPU: 4},
				WorkSpec{Units: 3, UnitDuration: 120 * time.Millisecond}})
		}
		for i := 0; i < 2; i++ {
			batch = append(batch, sub{"nlp", fmt.Sprintf("nlp-eval-%d-%d", round, i), "nlp-eval-batch",
				Resources{CPU: 2, MemMB: 4 * 1024, GPU: 0},
				WorkSpec{Units: 2, UnitDuration: 60 * time.Millisecond}})
			batch = append(batch, sub{"rec", fmt.Sprintf("rec-eval-%d-%d", round, i), "rec-ab-eval",
				Resources{CPU: 4, MemMB: 6 * 1024, GPU: 0},
				WorkSpec{Units: 2, UnitDuration: 70 * time.Millisecond}})
			batch = append(batch, sub{"data", fmt.Sprintf("data-prep-%d-%d", round, i), "data-preprocess",
				Resources{CPU: 8, MemMB: 16 * 1024, GPU: 0},
				WorkSpec{Units: 2, UnitDuration: 80 * time.Millisecond}})
		}
	}
	for _, b := range batch {
		_, _, err := eng.Submit(ctx, SubmitRequest{
			Tenant: b.tenant, IdemKey: b.key, Name: b.name,
			Want: b.want, Work: b.work, MaxRetries: 3,
		})
		if err != nil {
			return err
		}
	}
	fmt.Printf("  已提交 %d 个作业：cv(大训练, 4 卡) / nlp / rec(评测) / data(预处理)\n", len(batch))
	// 让 DRF 跑几个决策周期，展示调度顺序。
	sleepCtx(ctx, 700*time.Millisecond)
	printShareTable(eng)

	// -----------------------------------------------------------------
	section("场景 3：幂等提交 —— 同一 key 重复提交只入队一次")
	dupKey := "cv-train-0-0"
	dupWant := batch[0].want
	j1, created1, err := eng.Submit(ctx, SubmitRequest{Tenant: "cv", IdemKey: dupKey, Want: dupWant,
		Name: "cv-resnet-train", Work: batch[0].work, MaxRetries: 3})
	if err != nil {
		return err
	}
	j2, created2, err := eng.Submit(ctx, SubmitRequest{Tenant: "cv", IdemKey: dupKey, Want: dupWant,
		Name: "cv-resnet-train-DUPLICATE", Work: batch[0].work, MaxRetries: 3})
	if err != nil {
		return err
	}
	j3, created3, err := eng.Submit(ctx, SubmitRequest{Tenant: "rec", IdemKey: dupKey, Want: dupWant})
	if err != nil {
		fmt.Printf("  跨租户抢同 key 被拒: %v\n", err)
	} else {
		_, _ = j3, created3
	}
	fmt.Printf("  提交 #1: created=%v  state=%s\n", created1, j1.State())
	fmt.Printf("  提交 #2 (同 key): created=%v  same_handle=%v  name=%q\n",
		created2, j1 == j2, j2.Name)
	fmt.Printf("  → GPU 不会被双份占满：第二次提交返回的是同一个作业句柄，name 仍是 %q\n", j1.Name)

	// -----------------------------------------------------------------
	section("场景 4：依赖组 —— 上游失败，errgroup 取消下游")
	// 一个训练失败会拖垮它的下游预处理；另一个组成功跑完。
	grpFail := "grp-etl-fail"
	upFail := SubmitRequest{
		Tenant: "data", IdemKey: "etl-upstream-fail", Group: grpFail, Name: "etl-extract",
		Want: Resources{CPU: 4, MemMB: 8 * 1024, GPU: 0}, MaxRetries: 0,
		Work: WorkSpec{Units: 1, UnitDuration: 40 * time.Millisecond, FailAlways: true},
	}
	if _, _, err := eng.Submit(ctx, upFail); err != nil {
		return err
	}
	// 下游三个作业依赖上游；其中一个会跟上游一起被取消。
	for i := 0; i < 3; i++ {
		dep := SubmitRequest{
			Tenant: "data", IdemKey: fmt.Sprintf("etl-down-%d", i), Group: grpFail,
			Name: fmt.Sprintf("etl-transform-%d", i),
			Want: Resources{CPU: 2, MemMB: 4 * 1024, GPU: 0}, MaxRetries: 0,
			DepRefs: []DepRef{{IdemKey: "etl-upstream-fail"}},
			Work:    WorkSpec{Units: 4, UnitDuration: 200 * time.Millisecond},
		}
		if _, _, err := eng.Submit(ctx, dep); err != nil {
			return err
		}
	}
	// 一个成功的对照组：证明取消不是无差别的。
	grpOK := "grp-etl-ok"
	if _, _, err := eng.Submit(ctx, SubmitRequest{
		Tenant: "data", IdemKey: "etl-ok-upstream", Group: grpOK, Name: "etl-ok-extract",
		Want: Resources{CPU: 2, MemMB: 2 * 1024, GPU: 0}, MaxRetries: 0,
		Work: WorkSpec{Units: 1, UnitDuration: 30 * time.Millisecond},
	}); err != nil {
		return err
	}
	for i := 0; i < 2; i++ {
		if _, _, err := eng.Submit(ctx, SubmitRequest{
			Tenant: "data", IdemKey: fmt.Sprintf("etl-ok-down-%d", i), Group: grpOK,
			Name: fmt.Sprintf("etl-ok-transform-%d", i),
			Want: Resources{CPU: 1, MemMB: 2 * 1024, GPU: 0}, MaxRetries: 0,
			DepRefs: []DepRef{{IdemKey: "etl-ok-upstream"}},
			Work:    WorkSpec{Units: 2, UnitDuration: 50 * time.Millisecond},
		}); err != nil {
			return err
		}
	}
	fmt.Println("  已提交失败组 grp-etl-fail（1 上游必失败 + 3 下游）与成功组 grp-etl-ok（1 上游 + 2 下游）")
	// 等失败组的上游先跑完并触发取消，再等对照组自然跑完。
	sleepCtx(ctx, 1200*time.Millisecond)
	waitGroupsIdle(eng, 4*time.Second)
	reportDepGroups(eng, grpFail, grpOK)

	// -----------------------------------------------------------------
	section("场景 5：指数退避重试")
	retryKey := "rec-flaky"
	for i := 0; i < 4; i++ {
		if _, _, err := eng.Submit(ctx, SubmitRequest{
			Tenant: "rec", IdemKey: retryKey, Name: "rec-flaky-eval",
			Want: Resources{CPU: 2, MemMB: 4 * 1024, GPU: 0}, MaxRetries: 3,
			Work: WorkSpec{Units: 1, UnitDuration: 20 * time.Millisecond, FailFirst: 2},
		}); err != nil {
			return err
		}
	}
	fmt.Println("  rec-flaky 前 2 次尝试失败，之后成功；退避 80ms -> 160ms -> 320ms")
	sleepCtx(ctx, 1500*time.Millisecond)
	if j, ok := eng.sched.Lookup(retryKey); ok {
		fmt.Printf("  rec-flaky state=%s attempts=%d retries=%d\n", j.State(), j.Attempts(), j.Retries())
	} else {
		fmt.Printf("  rec-flaky 已结束（成功路径完成）\n")
	}
	rs := eng.runner.Stats()
	fmt.Printf("  累计重试次数=%d\n", rs.Retries)

	// -----------------------------------------------------------------
	section("场景 6：熔断 —— 连续失败 5 次熔断，半开放探活")
	breakerTenant := "nlp"
	var breakerKeys []string
	for i := 0; i < 7; i++ {
		k := fmt.Sprintf("nlp-broken-%d", i)
		breakerKeys = append(breakerKeys, k)
		if _, _, err := eng.Submit(ctx, SubmitRequest{
			Tenant: breakerTenant, IdemKey: k, Name: "nlp-broken-job",
			Want: Resources{CPU: 2, MemMB: 4 * 1024, GPU: 0}, MaxRetries: 0,
			Work: WorkSpec{Units: 1, UnitDuration: 20 * time.Millisecond, FailAlways: true},
		}); err != nil {
			return err
		}
	}
	fmt.Println("  nlp 连续提交 7 个必然失败的作业（不重试）")
	sleepCtx(ctx, 1500*time.Millisecond)
	b := eng.sched.Breaker(breakerTenant)
	fmt.Printf("  熔断器(nlp): %s\n", b)
	if until, open := b.breakerOpenUntil(); open {
		fmt.Printf("  熔断中，%s 后进入半开放；半开放只放 %d 个探测作业\n",
			time.Until(until).Round(time.Millisecond), breakerHalfOpenProbes)
	}
	// 熔断期间提交：应当被拒。
	rejKey := "nlp-during-open"
	if _, _, err := eng.Submit(ctx, SubmitRequest{
		Tenant: breakerTenant, IdemKey: rejKey, Name: "nlp-eval-during-open",
		Want: Resources{CPU: 2, MemMB: 4 * 1024, GPU: 0}, MaxRetries: 0,
		Work: WorkSpec{Units: 1, UnitDuration: 20 * time.Millisecond},
	}); err != nil {
		fmt.Printf("  熔断期间提交被拒: %v\n", err)
	}
	sleepCtx(ctx, 400*time.Millisecond)
	if j, ok := eng.sched.Lookup(rejKey); ok {
		fmt.Printf("  熔断期间作业 %s 仍为 %s（未被派发）\n", rejKey, j.State())
	} else {
		fmt.Printf("  熔断期间作业 %s 已结束\n", rejKey)
	}
	// 等冷却结束，半开放探活。
	until, open := b.breakerOpenUntil()
	if open {
		wait := time.Until(until) + 300*time.Millisecond
		fmt.Printf("  等待半开放窗口 (%s)...\n", wait.Round(time.Millisecond))
		sleepCtx(ctx, wait)
	}
	fmt.Printf("  半开放后熔断器: %s\n", b)
	probeKey := "nlp-halfopen-probe"
	if _, _, err := eng.Submit(ctx, SubmitRequest{
		Tenant: breakerTenant, IdemKey: probeKey, Name: "nlp-halfopen-probe",
		Want: Resources{CPU: 2, MemMB: 4 * 1024, GPU: 0}, MaxRetries: 0,
		Work: WorkSpec{Units: 1, UnitDuration: 20 * time.Millisecond},
	}); err != nil {
		return err
	}
	sleepCtx(ctx, 800*time.Millisecond)
	fmt.Printf("  探测作业 %s 提交后熔断器: %s（探测成功 -> closed）\n", probeKey, b)
	printBreakerTable(eng)

	// -----------------------------------------------------------------
	section("场景 7：调度前的最终资源占用")
	sleepCtx(ctx, 800*time.Millisecond)
	printShareTable(eng)
	printStatus(eng, false)

	// -----------------------------------------------------------------
	section("场景 8：优雅停机 —— 真实 SIGTERM")
	// 再提交几个长作业，确保停机时确实有在途任务需要排空。
	for i := 0; i < 4; i++ {
		if _, _, err := eng.Submit(ctx, SubmitRequest{
			Tenant: "cv", IdemKey: fmt.Sprintf("cv-long-%d", i), Group: "grp-long",
			Name: "cv-long-train",
			Want: Resources{CPU: 8, MemMB: 32 * 1024, GPU: 2}, MaxRetries: 0,
			Work: WorkSpec{Units: 8, UnitDuration: 150 * time.Millisecond},
		}); err != nil {
			return err
		}
	}
	sleepCtx(ctx, 250*time.Millisecond) // 让它们进入 running
	inFlight := eng.runner.InFlight()
	pendingAtStop := eng.sched.PendingCount()
	fmt.Printf("  停机前在途作业=%d 待调度=%d 资源=%s\n", inFlight, pendingAtStop, eng.pool.Snapshot())
	fmt.Printf("  向自身发送 SIGTERM（真实信号，非模拟）...\n")
	// 真实信号路径：os/signal 捕获 -> BeginDrain -> DrainAll(30s 预算)
	//             -> CancelPending(未开始) -> 取消残留 -> 最终 fsync。
	if err := selfSignal(); err != nil {
		return err
	}
	clean, detail := eng.Shutdown(context.Background())
	fmt.Printf("  停机结果: clean=%v detail=%s\n", clean, detail)
	fmt.Printf("  停机后资源池已全部归还: %s\n", eng.pool.Available())
	fmt.Printf("  WAL: %s\n", eng.wal.Stats())

	// -----------------------------------------------------------------
	section("场景 9：WAL 重放恢复 —— 模拟重启")
	// 上面的排空把 WAL 关掉了。为了演示「重启后重放恢复」，
	// 这里用一份新的 WAL 提交在途作业，然后不优雅退出（模拟崩溃），
	// 再用新引擎从日志恢复。
	crashWAL := *walPath + ".crash"
	_ = os.Remove(crashWAL)
	fmt.Println("  构建一份「崩溃现场」的 WAL：提交若干在途作业后不做优雅停机")
	crashOpts := schedOpts
	crashEngine, err := NewEngine(EngineOptions{
		WALPath:   crashWAL,
		PoolTotal: Resources{CPU: 64, MemMB: 256 * 1024, GPU: 8},
		Sched:     crashOpts,
		Tick:      3 * time.Millisecond,
		Logger:    nil, // 静默
		WorkFunc:  demoWorkFunc,
	})
	if err != nil {
		return err
	}
	crashEngine.StartNoSignal()
	// 提交：含一个依赖组与重复提交。
	crashJobs := []SubmitRequest{
		{Tenant: "cv", IdemKey: "recover-cv-train", Name: "recover-cv-train",
			Want: Resources{CPU: 16, MemMB: 48 * 1024, GPU: 4}, MaxRetries: 3,
			Work: WorkSpec{Units: 6, UnitDuration: 80 * time.Millisecond}},
		{Tenant: "nlp", IdemKey: "recover-nlp-eval", Name: "recover-nlp-eval",
			Want: Resources{CPU: 2, MemMB: 4 * 1024, GPU: 0}, MaxRetries: 3,
			Work: WorkSpec{Units: 3, UnitDuration: 60 * time.Millisecond}},
		{Tenant: "rec", IdemKey: "recover-rec-up", Name: "recover-rec-upstream",
			Want: Resources{CPU: 2, MemMB: 2 * 1024, GPU: 0}, MaxRetries: 3,
			Work: WorkSpec{Units: 2, UnitDuration: 40 * time.Millisecond}},
		{Tenant: "rec", IdemKey: "recover-rec-down", Name: "recover-rec-downstream",
			Want: Resources{CPU: 2, MemMB: 2 * 1024, GPU: 0}, MaxRetries: 3,
			DepRefs: []DepRef{{IdemKey: "recover-rec-up"}},
			Work:    WorkSpec{Units: 3, UnitDuration: 40 * time.Millisecond}},
	}
	for _, r := range crashJobs {
		if _, _, err := crashEngine.Submit(context.Background(), r); err != nil {
			return err
		}
	}
	// 重复提交一次，验证 WAL 里同 key 只落一条 submitted。
	if _, _, err := crashEngine.Submit(context.Background(), SubmitRequest{
		Tenant: "cv", IdemKey: "recover-cv-train",
		Want: Resources{CPU: 16, MemMB: 48 * 1024, GPU: 4},
		Work: WorkSpec{Units: 6, UnitDuration: 80 * time.Millisecond},
	}); err != nil {
		return err
	}
	sleepCtx(context.Background(), 200*time.Millisecond)
	crashPend := crashEngine.sched.PendingCount()
	// 硬关 WAL，不做 Flush/Close —— 模拟进程被 kill。
	crashEngine.wal.hardCloseForCrash()
	fmt.Printf("  崩溃前：待调度=%d 在途资源=%s（已 hard-close WAL，模拟 SIGKILL）\n",
		crashPend, crashEngine.pool.Snapshot())

	// 新引擎：从同一 WAL 恢复。
	fmt.Println("  启动新引擎，从 WAL 重放...")
	recoverEngine, err := NewEngine(EngineOptions{
		WALPath:   crashWAL,
		PoolTotal: Resources{CPU: 64, MemMB: 256 * 1024, GPU: 8},
		Sched:     crashOpts,
		Tick:      3 * time.Millisecond,
		Logger:    nil,
		WorkFunc:  demoWorkFunc,
	})
	if err != nil {
		return err
	}
	fmt.Printf("  恢复结果: WAL 重放记录=%d 重建在途作业=%d\n",
		recoverEngine.wal.ReplayedRecords(), recoverEngine.RecoveredCount())
	fmt.Printf("  重建后待调度=%d（依赖关系也一并恢复）\n", recoverEngine.sched.PendingCount())
	// 验证重复提交的 key 没有把作业变成两个。
	if j, ok := recoverEngine.sched.Lookup("recover-cv-train"); ok {
		fmt.Printf("  recover-cv-train 存在，state=%s name=%q（重复提交未产生第二份）\n", j.State(), j.Name)
	}
	// 让恢复出来的作业真正跑完，验证恢复后仍能正常调度。
	// 注意用 StartNoSignal：SIGTERM 在场景 8 已被第一个引擎消费掉，
	// 这里再注册进程级 handler 只会让信号投递到错误的对象上。
	recoverEngine.StartNoSignal()
	// 等所有恢复出来的作业真正跑完。
	//
	// 这里不能用 sched.PendingCount() 作为终止条件：它在作业刚被派发时
	// 就归零了（进入 running 不算 pending），此时执行才刚开始，
	// 于是循环立刻退出，统计出来只有 1 个成功——看起来像「只恢复了一个」。
	// 正确判据是「调度器里没有活跃作业，且执行器没有在途作业」。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if recoverEngine.sched.PendingCount() == 0 && recoverEngine.runner.InFlight() == 0 &&
			len(recoverEngine.runner.GroupNames()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	recStats := recoverEngine.runner.Stats()
	fmt.Printf("  恢复后执行结果: 成功=%d 失败=%d 取消=%d（重放作业全部正常跑完）\n",
		recStats.Successes, recStats.Failures, recStats.Canceled)
	_, _ = recoverEngine.Shutdown(context.Background())
	_ = os.Remove(crashWAL)

	// -----------------------------------------------------------------
	fmt.Println()
	fmt.Println("\033[1mdemo 完成。\033[0m")
	return nil
}

// demoWorkFunc 是 demo 用的执行体：正常路径等同 defaultWorkFunc，
// 但会显式在取消时把部分结果丢弃并返回 ctx 错误，保证取消可观测。
func demoWorkFunc(ctx context.Context, job *Job) (Result, error) {
	units := job.Work.Units
	if units <= 0 {
		units = 1
	}
	per := job.Work.UnitDuration
	if per <= 0 {
		per = 20 * time.Millisecond
	}
	items := 0
	for i := 0; i < units; i++ {
		timer := time.NewTimer(per)
		select {
		case <-ctx.Done():
			timer.Stop()
			// 取消：丢弃已累积的部分结果，不外泄。
			return Result{}, ctx.Err()
		case <-timer.C:
		}
		items++
	}
	if job.Work.FailFirst > 0 && job.Attempts() <= job.Work.FailFirst {
		job.setLastError(fmt.Errorf("%w (attempt %d)", errScriptedFailure, job.Attempts()))
		return Result{JobKey: job.IdemKey, Items: items}, errScriptedFailure
	}
	if job.Work.FailAlways {
		job.setLastError(fmt.Errorf("%w (attempt %d)", errFatalWork, job.Attempts()))
		return Result{JobKey: job.IdemKey, Items: items}, errFatalWork
	}
	if err := job.transition(StateSucceeded, "demo work completed"); err != nil {
		return Result{JobKey: job.IdemKey, Items: items}, err
	}
	return Result{JobKey: job.IdemKey, Items: items}, nil
}

// waitGroupsIdle 等所有作业组结算完毕（最长 d），让汇报看到的是终态。
func waitGroupsIdle(e *Engine, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if e.sched.PendingCount() == 0 && e.runner.InFlight() == 0 && len(e.runner.GroupNames()) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func printShareTable(e *Engine) {
	shares := e.sched.Shares(e.pool)
	fmt.Println("  租户 DRF 主导份额（份额最低者下一轮优先调度）:")
	fmt.Printf("  %-8s %-24s %8s %6s %6s %6s\n", "TENANT", "USAGE", "SHARE", "PEND", "RUN", "COLD")
	for _, sh := range shares {
		cold := ""
		if sh.ColdStart {
			cold = "*"
		}
		fmt.Printf("  %-8s %-24s %8.4f %6d %6d %6s\n", sh.Tenant, sh.Usage.String(), sh.Share, sh.Pending, sh.Running, cold)
	}
}

func printBreakerTable(e *Engine) {
	now := time.Now()
	for _, name := range []string{"cv", "nlp", "rec", "data"} {
		b := e.sched.Breaker(name)
		v := b.View(now)
		if v.Trips == 0 && v.State == "closed" {
			continue // 未触发的不必刷屏
		}
		fmt.Printf("  %-6s state=%-10s consec=%d trips=%d probes=%d %s\n",
			name, v.State, v.Consec, v.Trips, v.Probes, v.OpenLeft)
	}
}

// reportDepGroups 汇报依赖组的最终状态，重点展示「上游失败 -> 下游被取消」。
func reportDepGroups(e *Engine, failGroup, okGroup string) {
	fmt.Printf("  失败组 %s:\n", failGroup)
	printGroupMembers(e, failGroup)
	fmt.Printf("  对照组 %s（上游成功，下游应正常完成）:\n", okGroup)
	printGroupMembers(e, okGroup)
}

// printGroupMembers 通过 WAL 与内存快照列出组内作业终态。
func printGroupMembers(e *Engine, group string) {
	// 终态优先从 WAL 读：作业一旦终结就会被调度器移出活跃索引，
	// 内存里已经查不到了。读 WAL 还能顺带证明「落盘的状态事件」本身是完整的。
	states := make(map[string]JobState)
	// 非 durable 记录可能还在写缓冲里，先 flush 一次保证读得到。
	_ = e.wal.Flush()
	records, _, err := readWALFile(e.wal.Path())
	if err != nil {
		fmt.Printf("    (读取 WAL 失败: %v)\n", err)
		return
	}
	for _, r := range records {
		if r.Group != group {
			continue
		}
		switch r.Kind {
		case evSubmitted:
			// 只有还没有任何终态事件时才记为 pending，
			// 否则会被后面那条 completed 覆盖掉、把成功显示成 pending。
			if _, seen := states[r.IdemKey]; !seen {
				states[r.IdemKey] = StatePending
			}
		case evCompleted:
			states[r.IdemKey] = StateSucceeded
		case evFailed:
			states[r.IdemKey] = StateFailed
		case evCanceled:
			states[r.IdemKey] = StateCanceled
		}
	}
	// 仍在内存中的作业（尚未终结）用实时状态覆盖。
	for _, name := range e.runner.GroupNames() {
		grp, ok := e.runner.Group(name)
		if !ok || grp.Name != group {
			continue
		}
		grp.mu.Lock()
		for k := range grp.memberKeys {
			if st, done := states[k]; !done || st == StatePending {
				if j, found := e.sched.Lookup(k); found {
					states[k] = j.State()
				}
			}
		}
		grp.mu.Unlock()
	}
	if len(states) == 0 {
		fmt.Println("    (无)")
		return
	}
	keys := make([]string, 0, len(states))
	for k := range states {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		mark := ".."
		switch states[k] {
		case StateSucceeded:
			mark = "ok"
		case StateCanceled:
			mark = "XX"
		case StateFailed:
			mark = "!!"
		}
		fmt.Printf("    [%s] %s -> %s\n", mark, k, states[k])
	}
}
