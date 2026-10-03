package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// Allotter 抽象资源池，供 runner 扣减/释放。生产实现是 *Pool。
type Allotter interface {
	Acquire(tenant, key string, want Resources) error
	Release(tenant, key string, held Resources)
}

// ---------------------------------------------------------------------------
// 作业组
// ---------------------------------------------------------------------------

// JobGroup 是一组共享一个 errgroup 上下文的作业。需求里的「依赖组」就是它：
// 组内任一作业失败会取消整组，兄弟任务必须在取消信号下排空并释放资源。
type JobGroup struct {
	Name   string
	Tenant string

	mu         sync.Mutex
	g          *errgroup.Group
	ctx        context.Context
	cancel     context.CancelFunc
	memberKeys map[string]struct{}
	// failErr 记录第一个（也就是触发取消的那个）错误。
	failErr error
	// done 在组终结时关闭，用于停机时等待排空。
	done chan struct{}
	// once 保证 finish 只跑一次。
	once sync.Once
	// completed / total 供 drain 观测进度。
	// 生命周期受 grp.mu 保护：
	//   sealed   本轮结算已封口，不再接收新成员；
	//   retireOnSeal 封口即退休 —— 只有「确定不会再有新成员」的组才设它。
	// pendingSettle 是「已跑完、待结算」的成员。依赖组的成员分批到达，
	// 它配合 Settle 的扫描让每一批都被正确结算一次。
	sealed        bool
	retireOnSeal  bool
	pendingSettle map[string]struct{}
	// attemptSeq 是本组的尝试序号，用来区分同一作业的多次尝试（退避重试）。
	attemptSeq int64
	completed  atomic.Int64
	total      atomic.Int64
	// memberWg 统计组内已 dispatch 但尚未返回的作业数。
	memberWg sync.WaitGroup
}

// RunFunc 是单个作业的执行体。实现方必须在 ctx 取消时尽快返回，
// 且必须把已经占用/产出的东西清理干净。
type RunFunc func(ctx context.Context, job *Job) (Result, error)

// Result 是作业的部分结果。errgroup 取消时，已经算出来的部分结果会被
// 显式丢弃（runner 返回时把 Result 清空），不允许泄漏给下游。
type Result struct {
	JobKey string            `json:"job_key"`
	Items  int               `json:"items"`
	Meta   map[string]string `json:"meta,omitempty"`
}

func newJobGroup(name, tenant string, parent context.Context) *JobGroup {
	ctx, cancel := context.WithCancel(parent)
	g, gctx := errgroup.WithContext(ctx)
	_ = gctx // 组内任务统一用 gctx，取消传播由 errgroup 负责
	return &JobGroup{
		Name:          name,
		Tenant:        tenant,
		g:             g,
		ctx:           ctx,
		cancel:        cancel,
		memberKeys:    make(map[string]struct{}),
		pendingSettle: make(map[string]struct{}),
		done:          make(chan struct{}),
	}
}

// Add 把一个作业登记进组并 fan-out 执行。
// 注意顺序：先登记 memberKeys（用于幂等加入），再 Add 到 errgroup。
func (grp *JobGroup) Add(job *Job, held Resources, run RunFunc) {
	grp.mu.Lock()
	// attemptSeq 从 1 开始：Job.groupAttempt 的零值是 0，表示「从未加入过任何组」。
	grp.attemptSeq++
	if job.inGroupAttempt(grp.attemptSeq) {
		grp.mu.Unlock()
		return
	}
	job.setGroupAttempt(grp.attemptSeq)
	//
	// 写成 if _, dup := memberKeys[key]; dup { return } 会让退避重试彻底失效：
	// 重试是同一个幂等键的第二次 Add，被这里直接吞掉，于是 Dispatch 明明
	// 计了数（Executions+1）却没有任何执行体真正跑，作业永远停在 running。
	// 正确做法：用 attemptSeq（每次 Add 自增）来区分不同的尝试。
	grp.memberKeys[job.IdemKey] = struct{}{}
	grp.total.Add(1)
	grp.memberWg.Add(1)
	grp.mu.Unlock()

	grp.g.Go(func() error {
		// 注意这几个 defer 必须写在函数最开头。
		//
		// 这里踩过两个很隐蔽的坑：
		//  1. defer 是「注册时求值、执行时调用」；把 defer 写在 run() 调用之后，
		//     等于在 run 已经返回之后才注册，函数随即返回，defer 仍然会执行，
		//     但完成/待结算的登记时机就晚到了 reaper 判定之后，整组永远漏结算。
		//  2. 多个 defer 按 LIFO 执行：memberWg.Done 必须最先注册（最后执行），
		//     以保证「作业真的完全收尾」之后才对外宣告完成。
		defer grp.markFinished(job.IdemKey)
		defer grp.completed.Add(1)
		defer grp.memberWg.Done()

		// 每个作业从组上下文派生自己的 ctx。组被取消时，这里立刻收到信号。
		jobCtx, cancelJob := context.WithCancel(grp.ctx)
		defer cancelJob()

		res, err := run(jobCtx, job)
		if err != nil {
			// 关键的一行：把失败写进 errgroup。errgroup 会立刻 cancel
			// 组上下文，所有兄弟任务的 jobCtx 同时收到取消信号。
			return fmt.Errorf("group %s: job %s: %w", grp.Name, job.IdemKey, err)
		}
		// 成功产出的结果在作业层面单独交付，runner 层不聚合，
		// 避免「一个失败把别人的部分结果也带出去」。
		_ = res
		return nil
	})
	grp.mu.Lock()
	grp.mu.Unlock()
	_ = held
}

// Wait 等待组内全部作业排空，返回首个错误。
func (grp *JobGroup) Wait() error {
	return grp.g.Wait()
}

// WaitTimeout 等组排空，超时则强制取消残留任务。
// 返回 timeout=true 表示没能排干净。
func (grp *JobGroup) WaitTimeout(d time.Duration) (timeout bool) {
	// 组内已无在途作业时直接返回，不要白白把整个排空预算耗在这里。
	// 否则 DrainAll 会在「其实早就排干净了」的情况下反复走到超时分支，
	// 把 30 秒预算白白耗尽并误报 timeout。
	if grp.InFlight() == 0 {
		return false
	}
	finished := make(chan error, 1)
	go func() { finished <- grp.g.Wait() }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-finished:
		_ = err
		return false
	case <-t.C:
		grp.cancel() // 取消残留任务
		// 给一点缓冲让它们真正退出，再判定为超时。
		select {
		case <-finished:
			return true
		case <-time.After(2 * time.Second):
			return true
		}
	}
}

// Cancel 主动取消整组（例如上游失败、优雅停机开始）。
func (grp *JobGroup) Cancel() { grp.cancel() }

// Done 在组终结后关闭。
func (grp *JobGroup) Done() <-chan struct{} { return grp.done }

// finished 判断组内是否已无在途作业，可安全 Settle。
func (grp *JobGroup) finished() bool { return grp.InFlight() == 0 }

// seal 封口本组：不再接收新成员。
func (grp *JobGroup) seal() {
	grp.mu.Lock()
	grp.sealed = true
	grp.mu.Unlock()
}

// sealAndRetire 封口并标记退休。退休的组结算完就从索引里删除，
// 因为确定不会再有新成员进来（排空阶段、solo 组）。
func (grp *JobGroup) sealAndRetire() {
	grp.mu.Lock()
	grp.sealed = true
	grp.retireOnSeal = true
	grp.mu.Unlock()
}

// retired 报告该组是否已退休。
func (grp *JobGroup) retired() bool {
	grp.mu.Lock()
	defer grp.mu.Unlock()
	return grp.retireOnSeal
}

// reopen 在有新成员加入时解开封口与结算标记。
//
// 依赖组会出现这种「复活」：上游跑完的瞬间组内已无在途，reaper 就把它结算了，
// 而下游作业此刻才被放行派发。若不解开，reopen 后加入的新成员将无人结算。
func (grp *JobGroup) reopen() {
	grp.mu.Lock()
	grp.sealed = false
	grp.retireOnSeal = false
	grp.mu.Unlock()
}

// MarkDone 关闭 done 通道，由 Settle 在结算完成后调用。
func (grp *JobGroup) MarkDone() {
	grp.once.Do(func() { close(grp.done) })
}

// drainFinished 取出「已返回且尚未结算」的成员幂等键，并就地标记为已结算。
// 用 taken 而非计数，是因为依赖组的成员是分批到达的：
// 上游先结算完，下游稍后才加入并跑完，每一批都必须各自被结算一次。
func (grp *JobGroup) drainFinished() []string {
	grp.mu.Lock()
	defer grp.mu.Unlock()
	var out []string
	for k := range grp.pendingSettle {
		out = append(out, k)
		delete(grp.pendingSettle, k)
	}
	return out
}

// markFinished 由作业执行体返回时调用，登记该成员待结算。
// 结算触发由 Runner.Settle 扫描 pendingSettle 决定，不依赖任何计数器，
// 从根本上避免「计数归零早于真正跑完」这类竞态。
func (grp *JobGroup) markFinished(key string) {
	grp.mu.Lock()
	grp.pendingSettle[key] = struct{}{}
	grp.mu.Unlock()
}

// settleKeyDone 在结算完某个成员后把它移出待结算集合。
func (grp *JobGroup) settleKeyDone(key string) {
	grp.mu.Lock()
	delete(grp.pendingSettle, key)
	grp.mu.Unlock()
}

// hasPendingSettle 报告是否还有成员等待结算。
func (grp *JobGroup) hasPendingSettle() bool {
	grp.mu.Lock()
	defer grp.mu.Unlock()
	return len(grp.pendingSettle) > 0
}

// InFlight 返回组内尚未返回的作业数。
func (grp *JobGroup) InFlight() int64 {
	t := grp.total.Load()
	c := grp.completed.Load()
	if c > t {
		c = t
	}
	return t - c
}

// ---------------------------------------------------------------------------
// 退避
// ---------------------------------------------------------------------------

// backoff 是指数退避计算器：base * 2^attempt，带上限。
func backoff(base time.Duration, attempt int, cap time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	if attempt < 0 {
		attempt = 0
	}
	// 用幂运算前先夹住指数，避免 attempt 很大时溢出。
	shift := attempt
	if shift > 20 {
		shift = 20
	}
	d := time.Duration(float64(base) * math.Pow(2, float64(shift)))
	if d <= 0 || d > cap {
		d = cap
	}
	return d
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// Runner 负责把调度决策落到 errgroup 执行组上，并把执行结果反馈回调度器、
// 资源池与 WAL。
//
// 三个关键机制都在这里：
//  1. errgroup 取消传播（组内失败 → 兄弟任务收到 ctx 取消）；
//  2. 同键互斥（keyedMutex 执行期闸门，防并发双跑）；
//  3. 退避重试 + 熔断回报。
type Runner struct {
	allotter Allotter
	sched    *Scheduler
	wal      *WAL

	backoffBase time.Duration
	backoffCap  time.Duration
	maxRetries  int

	// groups 是活跃作业组，key 为组名。停机时统一排空。
	groupsMu sync.Mutex
	groups   map[string]*JobGroup

	// held 记录每个运行中作业已扣减的资源，release 幂等由 Pool 保证。
	heldMu sync.Mutex
	held   map[string]heldRef

	// depsIn 是「谁依赖我」的索引。上游终结时用来触发下游取消判定。
	depsMu sync.Mutex
	deps   map[string][]*Job

	km *keyedMutex

	// log 是日志出口，由 main 注入，便于测试静音。
	log Logger

	// ---- 统计 ----
	executions    atomic.Int64
	successes     atomic.Int64
	failures      atomic.Int64
	canceledRuns  atomic.Int64
	retries       atomic.Int64
	doubleRunSkip atomic.Int64
	releaseCount  atomic.Int64
}

func NewRunner(allotter Allotter, sched *Scheduler, wal *WAL) *Runner {
	return &Runner{
		allotter:    allotter,
		sched:       sched,
		wal:         wal,
		backoffBase: 50 * time.Millisecond,
		backoffCap:  2 * time.Second,
		groups:      make(map[string]*JobGroup),
		held:        make(map[string]heldRef),
		deps:        make(map[string][]*Job),
		km:          sched.km,
	}
}

func (r *Runner) SetBackoff(base, capTime time.Duration) {
	r.backoffBase = base
	r.backoffCap = capTime
}

// Logger 是 Runner/Scheduler 的日志出口。
type Logger func(format string, args ...any)

// SetLogger 注入日志出口。传 nil 表示静音。
func (r *Runner) SetLogger(l Logger) { r.log = l }

func (r *Runner) logf(format string, args ...any) {
	if r.log != nil {
		r.log(format, args...)
	}
}

// walEvent 把一条状态事件写进 WAL。durability=false 表示该事件可由
// 重放推导（退避重试），不强制每次 fsync。
//
// 记录里必须带上作业身份（key/group/name），否则重放和事后审计都无法把
// 终态事件归到具体作业上。
func (r *Runner) walEvent(rec walRecord, durability bool) {
	if r.wal == nil {
		return
	}
	if rec.TS == 0 {
		rec.TS = time.Now().UnixNano()
	}
	_ = r.wal.Append(rec, durability)
}

// jobRecord 以某个作业为主体构造一条 WAL 事件。
func (r *Runner) jobRecord(kind evKind, job *Job) walRecord {
	return walRecord{
		Kind:       kind,
		IdemKey:    job.IdemKey,
		Tenant:     job.Tenant,
		Group:      job.Group,
		Name:       job.Name,
		Want:       job.Want,
		DepRefs:    job.DepRefs,
		MaxRetries: job.MaxRetries,
		Work:       job.Work,
		Attempts:   job.Attempts(),
		RetryCount: job.Retries(),
	}
}

// Dispatch 是「调度决策 → 执行」的转换点。
func (r *Runner) Dispatch(parent context.Context, job *Job, run RunFunc) {
	// 同键互斥闸门：如果同一幂等键已经有一个执行体在跑，直接拒绝这次启动。
	// 这是防「手抖重复提交 → GPU 双份占满」的最后一道防线。
	releaseKey, ok := r.km.acquire(job.IdemKey)
	if !ok {
		r.doubleRunSkip.Add(1)
		r.logf("WARN job=%s tenant=%s same idempotency key already running, refusing concurrent double-run", job.IdemKey, job.Tenant)
		// 释放它已经扣下来的资源，避免白占。
		r.release(job.IdemKey)
		r.sched.Finish(job, StateCanceled, errors.New("duplicate concurrent run for same idempotency key"))
		rec := r.jobRecord(evCanceled, job)
		rec.Err = "duplicate concurrent run for same idempotency key (keyed mutex)"
		r.walEvent(rec, false)
		return
	}

	r.heldMu.Lock()
	r.held[job.IdemKey] = heldRef{Tenant: job.Tenant, Res: job.Want}
	r.heldMu.Unlock()

	grp := r.groupFor(parent, job)
	r.registerDeps(job)
	r.executions.Add(1)

	grp.Add(job, job.Want, func(ctx context.Context, j *Job) (Result, error) {
		// 无论走哪条路径，keyedMutex 与资源都必须归还，且只归还一次。
		var once sync.Once
		cleanup := func(held Resources) {
			once.Do(func() {
				releaseKey()
				r.heldMu.Lock()
				delete(r.held, j.IdemKey)
				r.heldMu.Unlock()
				r.releaseWith(j.IdemKey, j.ReleaseHold, held)
				r.unregisterDeps(j)
			})
		}
		// 兜底：panic 不能让资源泄漏。
		defer func() {
			if rec := recover(); rec != nil {
				cleanup(job.Want)
				panic(rec)
			}
		}()

		res, err := run(ctx, j)
		cleanup(job.Want)

		if err != nil {
			// ctx 被取消 = 上游/兄弟失败了，不是本作业的错。
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				r.canceledRuns.Add(1)
				return res, err
			}
			// 判断是否还能重试。
			retry := j.Retries() < j.MaxRetries
			if retry {
				r.retries.Add(1)
				return res, fmt.Errorf("%w (retryable, attempt %d/%d)", err, j.Retries()+1, j.MaxRetries+1)
			}
			return res, fmt.Errorf("%w (retries exhausted)", err)
		}
		r.successes.Add(1)
		return res, nil
	})
}

// groupFor 取（或建）作业所属的 errgroup。
//
// 这里必须复用「已结算但仍有在途后续成员」的组。依赖组的形态是：
// 上游先跑完 -> 组内暂时 InFlight()==0 -> reaper 判定可结算并把它从索引里
// 删除 -> 下游此时才被依赖判定放行并派发，于是 groupFor 建了一个同名的新组。
// 结果是下游作业跑完之后再没有任何人结算它，completed 事件永远不进 WAL，
// 租户账本也永远不减。修复方式：Settle 只在组「彻底退休」时才 forgetGroup，
// 而组是否退休由引擎根据「该组名是否已不可能再有新成员」来判断。
func (r *Runner) groupFor(parent context.Context, job *Job) *JobGroup {
	name := job.Group
	if name == "" {
		// 无组作业按幂等键自成一组，天然互不影响。
		name = "solo-" + job.IdemKey
	}
	r.groupsMu.Lock()
	defer r.groupsMu.Unlock()
	grp, ok := r.groups[name]
	if !ok {
		grp = newJobGroup(name, job.Tenant, parent)
		r.groups[name] = grp
	}
	// 复用已结算过的组时，必须解开结算锁与封口标记，
	// 让新成员还能加进来并被再次结算。
	grp.reopen()
	return grp
}

// Group 取出某个作业组（测试与 demo 用）。
func (r *Runner) Group(name string) (*JobGroup, bool) {
	r.groupsMu.Lock()
	defer r.groupsMu.Unlock()
	g, ok := r.groups[name]
	return g, ok
}

// GroupNames 返回所有活跃组名。
func (r *Runner) GroupNames() []string {
	r.groupsMu.Lock()
	defer r.groupsMu.Unlock()
	out := make([]string, 0, len(r.groups))
	for n := range r.groups {
		out = append(out, n)
	}
	return out
}

func (r *Runner) forgetGroup(name string) {
	r.groupsMu.Lock()
	delete(r.groups, name)
	r.groupsMu.Unlock()
}

// ---------------------------------------------------------------------------
// 结果处理（由主循环在组 Wait 之后调用）
// ---------------------------------------------------------------------------

// Settle 在作业组排空后，把每个作业的最终状态落到调度器 + WAL 上。
//
// 这里是熔断器唯一被喂数据的地方：它按租户统计「连续失败」，
// 达到阈值就熔断；熔断到期后半开放一个探测作业探活。
// settled 保证同一组只被结算一次（reaper 与 drainer 都可能看到它已排空）。
func (r *Runner) Settle(grp *JobGroup) {
	// 只结算「本轮刚跑完」的成员。
	keys := grp.drainFinished()
	if len(keys) == 0 {
		grp.MarkDone()
		return
	}

	now := time.Now()
	tenantResults := make(map[string]tenantOutcome)

	for _, key := range keys {
		job, ok := r.sched.Lookup(key)
		if !ok {
			continue // 已经结过了（例如被依赖判定连带取消）
		}
		outcome := r.settleJob(job)
		grp.settleKeyDone(job.IdemKey)
		tenantResults[job.Tenant] = mergeOutcome(tenantResults[job.Tenant], outcome)
	}

	// 按租户回报熔断器。
	for tenant, res := range tenantResults {
		b := r.sched.Breaker(tenant)
		tripped, state := b.Record(res.ok, res.canceled, now)
		if tripped {
			r.logf("BREAKER tenant=%s state=%s consec=%d cooldown=%s (熔断：连续失败达阈值)",
				tenant, state, breakerFailureThreshold, b.cooldown)
			r.walEvent(walRecord{
				Kind: evBreakerTrip, Tenant: tenant,
				Err: fmt.Sprintf("state=%s cooldown=%s", state, b.cooldown),
			}, false)
		}
	}
	grp.MarkDone()
	if grp.retired() {
		r.forgetGroup(grp.Name)
	}
}

// tenantOutcome 汇总某租户在一次 Settle 中的表现。
type tenantOutcome struct {
	ok       bool // 该租户是否有任何一个作业成功
	anyFail  bool
	canceled bool
}

func mergeOutcome(a, b tenantOutcome) tenantOutcome {
	return tenantOutcome{ok: a.ok || b.ok, anyFail: a.anyFail || b.anyFail, canceled: a.canceled || b.canceled}
}

// settleJob 判定单个作业的终态：成功 / 取消 / 失败 / 回队重试。
func (r *Runner) settleJob(job *Job) tenantOutcome {
	// 被取消（上游/兄弟失败传播，或停机超时强制取消）。
	if canceled, why := job.isCanceled(); canceled {
		r.sched.Finish(job, StateCanceled, errors.New(why))
		rec := r.jobRecord(evCanceled, job)
		rec.Err = why
		r.walEvent(rec, true)
		r.canceledRuns.Add(1)
		return tenantOutcome{canceled: true}
	}

	err := job.LastError()

	// 成功：写终态 WAL，让重放知道这个作业已经完成，不必再跑一遍。
	//
	// 这里必须调用 sched.Finish：它是唯一会把该租户的 reserved 减回去、
	// 把作业从活跃索引摘掉的出口。漏掉它会导致「跑完的作业仍然算在占用里」，
	// 于是调度器侧的份额无限累积（曾经出现 cv 记了 16 张卡 / 池只有 8 张），
	// 而资源池那边其实早就归还了——两边账本对不上，DRF 彻底失灵。
	if err == nil && job.State() == StateSucceeded {
		r.sched.Finish(job, StateSucceeded, nil)
		r.walEvent(r.jobRecord(evCompleted, job), false)
		return tenantOutcome{ok: true}
	}

	// 可重试：指数退避后重新入队。
	if err != nil && !isCancelErr(err) && job.Retries() < job.MaxRetries {
		delay := backoff(r.backoffBase, job.Retries(), r.backoffCap)
		job.setRetryCount(job.Retries() + 1)
		// 资源此刻已经还给池子了，记账必须同步减（见 NoteRetry 注释）。
		r.sched.NoteRetry(job)
		r.retries.Add(1)
		rec := r.jobRecord(evRetry, job)
		rec.Err = fmt.Sprintf("backoff=%s err=%v", delay.Round(time.Millisecond), err)
		r.walEvent(rec, false)
		r.logf("RETRY job=%s tenant=%s backoff=%s (%d/%d) err=%v",
			job.IdemKey, job.Tenant, delay.Round(time.Millisecond), job.Retries(), job.MaxRetries, err)

		// 退避计时不占用主循环：单独一个 goroutine 到点把作业放回就绪堆。
		go func(j *Job, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			<-t.C
			r.sched.Requeue(j)
		}(job, delay)
		return tenantOutcome{anyFail: true}
	}

	// 重试耗尽 / 被取消 / 状态异常：进入终态。
	//
	// 注意 err 可能是 nil：排空时组被强制取消，执行体返回的是
	// context.Canceled，但某些实现在取消路径上会直接返回 nil error。
	// 这时如果只看 err，就会把一个被取消的作业误判成 failed，
	// 还会打出 "err=<nil>" 这种让人以为有 bug 的日志。
	st := StateFailed
	switch {
	case job.State() == StateSucceeded:
		st = StateSucceeded
	case err == nil || isCancelErr(err):
		// 没有错误、也没有成功：只能是「被取消」。补一个明确的原因，
		// 让 WAL 和日志都留下可读的依据。
		st = StateCanceled
		if err == nil {
			err = errDrainedDuringShutdown
		}
	}
	r.sched.Finish(job, st, err)
	rec := r.jobRecord(evFailed, job)
	rec.Err = errString(err)
	if st == StateCanceled {
		rec.Kind = evCanceled
		r.canceledRuns.Add(1)
	} else {
		r.failures.Add(1)
	}
	r.walEvent(rec, true)
	r.logf("%s job=%s tenant=%s attempts=%d err=%v", st, job.IdemKey, job.Tenant, job.Attempts(), err)

	if st == StateSucceeded {
		return tenantOutcome{ok: true}
	}
	if st == StateCanceled {
		return tenantOutcome{canceled: true}
	}
	return tenantOutcome{anyFail: true}
}

// errDrainedDuringShutdown 表示作业在排空阶段被强制取消。
var errDrainedDuringShutdown = errors.New("canceled: forced drain at shutdown")

func isCancelErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// ---------------------------------------------------------------------------
// 依赖索引
// ---------------------------------------------------------------------------

func (r *Runner) registerDeps(job *Job) {
	if len(job.DepRefs) == 0 {
		return
	}
	r.depsMu.Lock()
	defer r.depsMu.Unlock()
	for _, d := range job.DepRefs {
		r.deps[d.IdemKey] = append(r.deps[d.IdemKey], job)
	}
}

func (r *Runner) unregisterDeps(job *Job) {
	if len(job.DepRefs) == 0 {
		return
	}
	r.depsMu.Lock()
	defer r.depsMu.Unlock()
	for _, d := range job.DepRefs {
		list := r.deps[d.IdemKey]
		for i, j := range list {
			if j == job {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(r.deps, d.IdemKey)
		} else {
			r.deps[d.IdemKey] = list
		}
	}
}

// ---------------------------------------------------------------------------
// 资源归还
// ---------------------------------------------------------------------------

// heldRef 是「已扣资源」的完整记录：租户 + 资源量。
type heldRef struct {
	Tenant string
	Res    Resources
}

func (r *Runner) release(key string) {
	r.heldMu.Lock()
	h, ok := r.held[key]
	if ok {
		delete(r.held, key)
	}
	r.heldMu.Unlock()
	if !ok {
		return
	}
	r.releaseWith(key, h.Tenant, h.Res)
}

// releaseWith 归还资源。
//
// 注意这里刻意「不查调度器」：作业终结后调度器会把它从索引里删掉，
// 而释放动作常常正好发生在终结的同一瞬间。去查索引就意味着
// 「持资源池锁 -> 持调度器锁」，与 Plan 路径的「持调度器锁 -> 资源池锁」
// 构成 AB-BA 死锁（这个 bug 真实发生过，表现为 dispatch 后进程挂死）。
// 租户信息由 Job.ReleaseHold 固化在作业上，释放路径完全无锁。
func (r *Runner) releaseWith(key string, tenant string, held Resources) {
	r.allotter.Release(tenant, key, held)
	r.releaseCount.Add(1)
}

// ---------------------------------------------------------------------------
// 排空
// ---------------------------------------------------------------------------

// DrainAll 在给定总预算内排空所有作业组，返回是否全部排空干净。
//
// 排空阶段调度已经停止，不会再有新成员进来，因此每个组最多只需要
// 「等一次 -> 结算一次 -> 退休」这三步。这里刻意写成单趟直线流程而不是轮询：
// 轮询版本在没有正确退休某些组时会退化成空转死循环，表现为停机时进程挂死。
//
// 判据是 in_flight 归零：提前排干净就立刻返回，预算只是上限而不是固定时长。
func (r *Runner) DrainAll(budget time.Duration) (clean bool, detail string) {
	deadline := time.Now().Add(budget)

	r.groupsMu.Lock()
	names := make([]string, 0, len(r.groups))
	for n := range r.groups {
		names = append(names, n)
	}
	r.groupsMu.Unlock()

	for _, n := range names {
		grp, ok := r.Group(n)
		if !ok {
			continue
		}
		// 排空期间不再有新成员：先封口并退休，避免排空完成后组还留在索引里。
		grp.sealAndRetire()

		if grp.InFlight() == 0 {
			r.Settle(grp) // 已排空，直接结算
			continue
		}

		// 给这个组剩下的全部预算；超时则强制取消残留任务。
		remaining := time.Until(deadline)
		if remaining <= 0 {
			grp.Cancel()
			r.Settle(grp)
			continue
		}
		if grp.WaitTimeout(remaining) {
			r.logf("DRAIN group=%s forced-cancel residual tasks", grp.Name)
		}
		r.Settle(grp)
	}

	// 收尾：把可能因为竞态而残留的组也强制结算掉。
	for _, n := range r.GroupNames() {
		if grp, ok := r.Group(n); ok {
			grp.sealAndRetire()
			grp.Cancel()
			r.Settle(grp)
		}
	}
	// 只要没有在途作业就算排空干净；组是否清空只是附带信息。
	if r.InFlight() == 0 {
		return true, r.drainSummary("clean")
	}
	return false, r.drainSummary("residual-in-flight")
}

func (r *Runner) drainSummary(reason string) string {
	var inflight int64
	r.groupsMu.Lock()
	for _, g := range r.groups {
		inflight += g.InFlight()
	}
	r.groupsMu.Unlock()
	return fmt.Sprintf("%s in_flight=%d", reason, inflight)
}

// InFlight 汇总所有组内未完成的作业数。
func (r *Runner) InFlight() int64 {
	r.groupsMu.Lock()
	defer r.groupsMu.Unlock()
	var n int64
	for _, g := range r.groups {
		n += g.InFlight()
	}
	return n
}

// CancelAllGroups 取消所有组（停机第一阶段或上游硬失败时用）。
func (r *Runner) CancelAllGroups() {
	r.groupsMu.Lock()
	defer r.groupsMu.Unlock()
	for _, g := range r.groups {
		g.Cancel()
	}
}

// ---------------------------------------------------------------------------
// 观测
// ---------------------------------------------------------------------------

type RunnerStats struct {
	Executions    int64 `json:"executions"`
	Successes     int64 `json:"successes"`
	Failures      int64 `json:"failures"`
	Canceled      int64 `json:"canceled"`
	Retries       int64 `json:"retries"`
	DoubleRunSkip int64 `json:"double_run_skipped"`
	Releases      int64 `json:"releases"`
	InFlight      int64 `json:"in_flight"`
	KeyedMuWaits  int64 `json:"keyed_mutex_waits"`
}

func (r *Runner) Stats() RunnerStats {
	return RunnerStats{
		Executions:    r.executions.Load(),
		Successes:     r.successes.Load(),
		Failures:      r.failures.Load(),
		Canceled:      r.canceledRuns.Load(),
		Retries:       r.retries.Load(),
		DoubleRunSkip: r.doubleRunSkip.Load(),
		Releases:      r.releaseCount.Load(),
		InFlight:      r.InFlight(),
		KeyedMuWaits:  r.km.Waits(),
	}
}
