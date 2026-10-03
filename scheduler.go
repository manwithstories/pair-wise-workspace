// scheduler.go — DRF 公平共享分配器核心 + 优先级队列 + 熔断器 + 幂等/依赖治理。
//
// DRF（Dominant Resource Fairness）算法要点：
//
//	对每个租户 t，令 u_t = 已用量_t / 总量（逐维占比），取其最大值为主导份额
//	dominant(t) = max(cpu占比, mem占比, gpu占比)。调度时挑 dominant 最小的租户，
//	使"最小化最大主导份额"这一全局目标被逐步逼近。
//
// 冷启动补偿：新租户的历史用量为 0，dominant 必然最低，会立刻抢到资源。
// 这正是 DRF 想要的短���公平，但为避免单个新租户瞬间吃满整机，这里叠加
// "每租户并发上限"与"GPU 单作业上限"，把突发限制在可控范围。
//
// 边界处理（题面要求逐条覆盖）：
//   - 请求为零/超额      : Resources.Validate 拦截零请求；TryReserve 拦截超额请求。
//   - 主导份额并列      : 用 (dominant, dominantCPU, dominantMem, dominantGPU, 提交序号)
//     复合排序，全部并列时退化为 FIFO（先提交先得，避免饥饿）。
//   - 依赖未满足        : 上游未成功的作业不进入调度候选（blocked）。
//   - 依赖已失败/取消   : 立刻级联取消下游，释放其资源（不留在队列里空占）。
//   - 退避未到/熔断中   : 不进入候选；熔断半开放只放一个探测作业。
package main

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// DefaultMaxAttempts 是作业默认最大尝试次数（题面：失败指数退避重试最多 3 次）。
const DefaultMaxAttempts = 3

// KindNoop 是无需水合的空任务（仅占位，用于演示）。
const KindNoop = "noop"

// 熔断器参数（题面：连续失败 5 次熔断 60 秒，半开放一个探测作业探活）。
const (
	BreakerFailThreshold = 5
	BreakerCooldown      = 60 * time.Second
	BreakerHalfOpenProbe = 1
)

// 退避基数（重试最多 3 次 => 1s / 2s / 4s 级别）。
const retryBackoffBase = 200 * time.Millisecond

// Errors returned by the scheduler.
var (
	ErrDuplicate   = errors.New("幂等键已存在，返回已有句柄")
	ErrCircuitOpen = errors.New("租户熔断中")
	ErrUnknownJob  = errors.New("作业不存在")
	ErrDraining    = errors.New("调度器正在优雅停机，停止接收新提交")
	ErrTooLarge    = errors.New("资源请求超出单池总量")
)

// ---- 优先级队列 ----------------------------------------------------------

// readyItem 是 DRF 优先队列的元素。
type readyItem struct {
	job    *Job
	tenant string
	score  float64 // 主导份额（越小越优先）
	seq    uint64  // FIFO 兜底
	idx    int     // heap 内部索引

	// 逐维占比，用于主导份额并列时的细分排序。
	dCPU, dMem, dGPU float64
}

// readyQueue 是基于 container/heap 的最小堆。
type readyQueue []*readyItem

func (rq readyQueue) Len() int { return len(rq) }

// Less：主导份额优先小者；并列时依次比 CPU/内存/GPU 占比，再并列则按提交序号 FIFO。
func (rq readyQueue) Less(a, b int) bool {
	x, y := rq[a], rq[b]
	if x.score != y.score {
		return x.score < y.score
	}
	if x.dCPU != y.dCPU {
		return x.dCPU < y.dCPU
	}
	if x.dMem != y.dMem {
		return x.dMem < y.dMem
	}
	if x.dGPU != y.dGPU {
		return x.dGPU < y.dGPU
	}
	return x.seq < y.seq
}

func (rq readyQueue) Swap(a, b int) {
	rq[a], rq[b] = rq[b], rq[a]
	rq[a].idx, rq[b].idx = a, b
}

func (rq *readyQueue) Push(x any) {
	it := x.(*readyItem)
	it.idx = len(*rq)
	*rq = append(*rq, it)
}

func (rq *readyQueue) Pop() any {
	old := *rq
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	it.idx = -1
	*rq = old[:n-1]
	return it
}

// ---- 熔断器 --------------------------------------------------------------

// breakerState 是单租户熔断器状态机：closed -> open -> halfOpen -> closed。
type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// tenantBreaker 是单租户熔断器。
type tenantBreaker struct {
	state     breakerState
	failures  int
	openedAt  time.Time
	probesOut int // 半开放期间已放出的探测作业数
}

func (b *tenantBreaker) allow(now time.Time) bool {
	switch b.state {
	case breakerClosed:
		return true
	case breakerOpen:
		if now.Sub(b.openedAt) >= BreakerCooldown {
			// 冷却结束 -> 半开放，放一个探测作业探活。
			b.state = breakerHalfOpen
			b.probesOut = 0
		} else {
			return false
		}
	}
	if b.state == breakerHalfOpen {
		if b.probesOut < BreakerHalfOpenProbe {
			b.probesOut++
			return true
		}
		return false
	}
	return true
}

func (b *tenantBreaker) onSuccess() {
	b.state = breakerClosed
	b.failures = 0
	b.probesOut = 0
}

func (b *tenantBreaker) onFailure(now time.Time) {
	if b.state == breakerHalfOpen {
		// 探活失败 -> 立刻回到 open，重新计时。
		b.state = breakerOpen
		b.openedAt = now
		b.probesOut = 0
		b.failures = BreakerFailThreshold
		return
	}
	b.failures++
	if b.failures >= BreakerFailThreshold {
		b.state = breakerOpen
		b.openedAt = now
		b.probesOut = 0
	}
}

// ---- 租户视图 ------------------------------------------------------------

// tenantView 是调度器为某租户维护的 DRF 账本。
type tenantView struct {
	name string
	// 已用量（与 Pool.TenantUsed 一致，用于份额计算，避免每次决策都进池的分片锁）。
	used Resources
	// 当前在途（已预留/运行）作业数，用于并发上限。
	running int
	breaker tenantBreaker
}

// dominant 计算租户的主导份额：逐维占比取最大。
// 分母为 0 的维度不参与（该资源未开放或请求为 0）。
func (tv *tenantView) dominant(total Resources) float64 {
	c, m, g := 0.0, 0.0, 0.0
	if total.CPU > 0 {
		c = float64(tv.used.CPU) / float64(total.CPU)
	}
	if total.Mem > 0 {
		m = float64(tv.used.Mem) / float64(total.Mem)
	}
	if total.GPU > 0 {
		g = float64(tv.used.GPU) / float64(total.GPU)
	}
	return max3(c, m, g)
}

func (tv *tenantView) dimRatios(total Resources) (dCPU, dMem, dGPU float64) {
	if total.CPU > 0 {
		dCPU = float64(tv.used.CPU) / float64(total.CPU)
	}
	if total.Mem > 0 {
		dMem = float64(tv.used.Mem) / float64(total.Mem)
	}
	if total.GPU > 0 {
		dGPU = float64(tv.used.GPU) / float64(total.GPU)
	}
	return
}

func max3(a, b, c float64) float64 {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}

// ---- Scheduler ------------------------------------------------------------

// Scheduler 是调度器核心：幂等去重 -> WAL -> DRF 优先队列 -> errgroup 执行。
type Scheduler struct {
	pool *Pool
	wal  *WAL

	mu        sync.Mutex // 保护以下所有内存结构
	tenants   map[string]*tenantView
	byID      map[string]*Job        // ID -> 作业
	byIdem    map[string]*Job        // 幂等键 -> 作业（同键只入队一次）
	ready     readyQueue             // DRF 最小堆
	groups    map[string]*groupState // 依赖组 -> errgroup 状态
	waiting   map[string][]string    // 下游作业 ID -> 阻塞原因（依赖未满足）
	unfit     []*Job                 // 因资源装不下被暂存、待资源释放后重试
	retryQ    []*Job                 // 指数退避中的待重试作业
	seq       uint64
	draining  bool
	drainOnce sync.Once

	runner *Runner // 执行器（AttachRunner 挂载）

	// 事件回调（终端打印调度轨迹）。
	trace func(TraceEvent)

	// wakeCh 唤醒调度循环（资源释放/退避到期/依赖满足时触发一次重新决策）。
	wakeCh chan struct{}

	// 运行期统计
	nAdmitted  int
	nCompleted int
	nFailed    int
	nCanceled  int
	nRejected  int
	nDuplicate int

	// 依赖组取消记录（组 ID -> 原因），用于重放/审计。
	groupCancelReason map[string]string
}

// groupState 记录一个依赖组的生命周期：一个 errgroup + 共享 context。
type groupState struct {
	id     string
	group  *errgroup.Group
	ctx    context.Context
	cancel context.CancelFunc // 组内任一失败 -> 触发，级联取消兄弟与下游
}

// TraceEvent 是调度轨迹事件（终端输出）。
type TraceEvent struct {
	TS     time.Time
	Kind   string // admit / start / ok / fail / retry / cancel / reject / dup / breaker
	JobID  string
	Tenant string
	Msg    string
	Used   Resources
}

// NewScheduler 创建调度器。
func NewScheduler(pool *Pool, wal *WAL, trace func(TraceEvent)) *Scheduler {
	if trace == nil {
		trace = func(TraceEvent) {}
	}
	return &Scheduler{
		pool: pool, wal: wal, trace: trace,
		tenants:           map[string]*tenantView{},
		byID:              map[string]*Job{},
		byIdem:            map[string]*Job{},
		groups:            map[string]*groupState{},
		waiting:           map[string][]string{},
		groupCancelReason: map[string]string{},
		ready:             readyQueue{},
		wakeCh:            make(chan struct{}, 1),
	}
}

// tenantLocked 取租户视图（调用方须持锁）。
func (s *Scheduler) tenantLocked(name string) *tenantView {
	tv, ok := s.tenants[name]
	if !ok {
		tv = &tenantView{name: name}
		s.tenants[name] = tv
	}
	return tv
}

// Submit 提交一个作业：幂等去重 -> 依赖校验 -> WAL 预写 -> 入 DRF 队列。
//
// 返回值 handle == true 表示这是新建的作业；handle == false 表示幂等命中，
// 返回的是已存在作业的句柄（题面：重复提交返回已存在句柄）。
func (s *Scheduler) Submit(spec JobSpec) (h *Job, created bool, err error) {
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return nil, false, ErrDraining
	}
	// 幂等去重：同键直接返回既有句柄，不再入队（防手抖重复提交双占 GPU）。
	if spec.IdemKey == "" {
		spec.IdemKey = spec.ID
	}
	if existing, ok := s.byIdem[spec.IdemKey]; ok {
		s.mu.Unlock()
		s.nDuplicate++
		s.trace(TraceEvent{Kind: "dup", JobID: existing.ID, Tenant: existing.Tenant,
			Msg: fmt.Sprintf("幂等键 %s 命中已有作业 %s，返回已有句柄", spec.IdemKey, existing.ID)})
		return existing, false, nil
	}
	if _, ok := s.byID[spec.ID]; ok {
		s.mu.Unlock()
		s.nDuplicate++
		return s.byID[spec.ID], false, nil
	}

	s.seq++
	now := time.Now()
	j, err := NewJob(spec, s.seq, now)
	if err != nil {
		s.mu.Unlock()
		return nil, false, err
	}
	// 未显式给执行体时，按 Kind 从注册表取（与 WAL 重放水合走同一张表），
	// 保证"提交即真实执行"，而不是静默变成空任务。
	if j.Run == nil {
		if fn, ok := taskRegistry[j.Kind]; ok {
			j.Run = fn
		} else {
			j.Run = noopTask
		}
	}

	// 超额请求直接拒（避免它永久占队列却永远装不下）。
	total := s.pool.Total()
	if !j.Req.Fits(total, Resources{}) {
		s.mu.Unlock()
		s.nRejected++
		_ = s.wal.Append(WLEvent{Event: EvtRejected, Reason: ErrTooLarge.Error(), Job: record(j)})
		return nil, false, fmt.Errorf("%w: %s 需 %s，池总量 %s", ErrTooLarge, j.ID, j.Req, total)
	}

	// 依赖校验：上游必须已存在。
	for _, dep := range j.DependsOn {
		if _, ok := s.byID[dep]; !ok {
			s.mu.Unlock()
			return nil, false, fmt.Errorf("依赖的上游作业 %q 不存在或未提交", dep)
		}
	}

	s.byID[j.ID] = j
	s.byIdem[j.IdemKey] = j
	tv := s.tenantLocked(j.Tenant)

	// WAL 预写：先落盘再入队，崩溃后可恢复。
	_ = s.wal.Append(WLEvent{Event: EvtSubmit, Job: record(j)})

	// 依赖阻塞判断：上游全部成功才可调度。
	blocked, reason := s.dependencyBlockedLocked(j)
	if blocked {
		s.waiting[j.ID] = append(s.waiting[j.ID], reason)
		s.mu.Unlock()
		s.trace(TraceEvent{Kind: "block", JobID: j.ID, Tenant: j.Tenant,
			Msg: "依赖未满足: " + reason})
		return j, true, nil
	}

	// 入 DRF 优先队列（冷启动补偿：dominant=0 的新租户天然优先）。
	s.pushReadyLocked(j, tv)
	_ = s.wal.Append(WLEvent{Event: EvtAdmitted, Job: record(j)})
	s.mu.Unlock()
	s.trace(TraceEvent{Kind: "admit", JobID: j.ID, Tenant: j.Tenant,
		Msg: fmt.Sprintf("入队 请求=%s", j.Req)})
	return j, true, nil
}

// pushReadyLocked 把作业推入 DRF 最小堆（调用方须持锁）。
func (s *Scheduler) pushReadyLocked(j *Job, tv *tenantView) {
	d := tv.dominant(s.pool.Total())
	dCPU, dMem, dGPU := tv.dimRatios(s.pool.Total())
	it := &readyItem{job: j, tenant: j.Tenant, score: d, seq: j.Seq}
	it.dCPU, it.dMem, it.dGPU = dCPU, dMem, dGPU
	heap.Push(&s.ready, it)
	j.mu.Lock()
	j.ready = true
	j.mu.Unlock()
}

// dependencyBlockedLocked 判断作业是否被依赖阻塞。
// 返回 (blocked, reason)。
func (s *Scheduler) dependencyBlockedLocked(j *Job) (bool, string) {
	for _, dep := range j.DependsOn {
		up, ok := s.byID[dep]
		if !ok {
			return true, "上游 " + dep + " 不存在"
		}
		st := up.StateOf()
		if st == StateSucceeded {
			continue
		}
		if st.Terminal() {
			// 上游失败/取消 -> 级联：本作业直接取消，不入队。
			return true, "上游 " + dep + " 处于 " + string(st) + "，级联取消"
		}
		return true, "等待上游 " + dep + "（当前 " + string(st) + "）"
	}
	return false, ""
}

// ---- 决策循环 ------------------------------------------------------------

// tick 是调度器的一次决策循环：尽可能填满资源池。
// 由 dispatcher 循环或测试直接驱动。
func (s *Scheduler) tick() {
	for {
		s.mu.Lock()
		now := time.Now()

		// 1) 处理退避到期 / 熔断半开放 -> 重新入队
		s.promoteEligibleLocked(now)

		// 2) 从 DRF 堆里挑作业
		item, lease, ok := s.pickLocked(now)
		if !ok {
			s.mu.Unlock()
			return
		}
		j := item.job

		// 3) 状态机 pending -> running
		if err := j.transition(StateRunning); err != nil {
			s.mu.Unlock()
			continue
		}
		j.mu.Lock()
		j.StartedAt = now
		j.mu.Unlock()
		tv := s.tenantLocked(j.Tenant)
		tv.running++
		_ = s.wal.Append(WLEvent{Event: EvtRunning, Job: record(j)})
		s.mu.Unlock()

		// 4) 启动执行（解锁后，避免在 goroutine 中持调度锁）
		s.start(jobHandle{j: j, lease: lease})
	}
}

// jobHandle 是"作业 + 已预留租约"的打包，交给 runner 执行。
type jobHandle struct {
	j     *Job
	lease Lease
}

// pickLocked 从 DRF 堆顶选出下一个可执行作业并完成资源预留。
// 关键：不能真正启动的候选（依赖阻塞、退避未到、熔断、装不下）会被跳过或降级处理。
func (s *Scheduler) pickLocked(now time.Time) (*readyItem, Lease, bool) {
	for s.ready.Len() > 0 {
		it := (*readyItem)(s.ready[0])
		j := it.job

		// 堆顶作业若不再可调度（取消/终态），直接弹出丢弃。
		st := j.StateOf()
		if st != StatePending {
			heap.Pop(&s.ready)
			j.mu.Lock()
			j.ready = false
			j.mu.Unlock()
			continue
		}

		// 退避未到 -> 把它从堆里挪走（后面再放回来），避免卡住堆顶阻塞其它租户。
		j.mu.Lock()
		notBefore := j.NotBefore
		j.mu.Unlock()
		if now.Before(notBefore) {
			heap.Pop(&s.ready)
			j.mu.Lock()
			j.ready = false
			j.mu.Unlock()
			continue
		}

		// 依赖未满足 / 上游失败 -> 不调度。
		if blocked, reason := s.dependencyBlockedLocked(j); blocked {
			if isCascade(reason) {
				// 上游已终态失败：级联取消本作业。
				heap.Pop(&s.ready)
				s.cancelCascadeLocked(j, reason)
				continue
			}
			// 只是"等待上游" -> 移出堆，等上游完成时再 promote。
			heap.Pop(&s.ready)
			j.mu.Lock()
			j.ready = false
			j.mu.Unlock()
			s.waiting[j.ID] = append(s.waiting[j.ID], reason)
			continue
		}

		// 熔断：跳过并丢弃本次机会（不丢作业，作业已在堆外，等待熔断恢复）。
		tv := s.tenantLocked(j.Tenant)
		if !tv.breaker.allow(now) {
			heap.Pop(&s.ready)
			j.mu.Lock()
			j.ready = false
			j.mu.Unlock()
			// 作业暂不可调度：记录退避到熔断冷却结束，避免立刻又回堆。
			j.setNotBefore(now.Add(BreakerCooldown))
			continue
		}

		// 尝试资源预留。装不下 -> 把该作业暂存，换下一个候选（后面再重试）。
		lease, err := s.pool.TryReserve(j.Tenant, j.Req)
		if err != nil {
			// 装不下：把作业移出堆，暂存到 retryBucket（资源释放时再 promote）。
			heap.Pop(&s.ready)
			j.mu.Lock()
			j.ready = false
			j.mu.Unlock()
			s.unfit = append(s.unfit, j)
			continue
		}
		// 预留成功：从堆顶摘下（已启动，不再是候选）。
		heap.Pop(&s.ready)
		j.mu.Lock()
		j.ready = false
		j.mu.Unlock()

		// 同步租户账本（Pool 已记账，这里只同步调度器视图）。
		tv.used = s.pool.TenantUsed(j.Tenant)
		lease.Commit()
		return it, lease, true
	}
	return nil, Lease{}, false
}

// isCascade 判断阻塞原因是否为"上游终态失败导致级联取消"。
func isCascade(reason string) bool {
	return len(reason) > 0 && contains(reason, "级联取消")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// promoteEligibleLocked 把退避到期 / 资源已释放的作业重新推回 DRF 堆。
func (s *Scheduler) promoteEligibleLocked(now time.Time) {
	// 1) unfit 桶：之前因为装不下被暂存的，重新试。
	for i := 0; i < len(s.unfit); i++ {
		j := s.unfit[i]
		j.mu.Lock()
		notBefore, state, inReady := j.NotBefore, j.State, j.ready
		j.mu.Unlock()
		if state != StatePending || inReady {
			s.unfit[i] = s.unfit[len(s.unfit)-1]
			s.unfit = s.unfit[:len(s.unfit)-1]
			i--
			continue
		}
		if now.Before(notBefore) {
			continue
		}
		tv := s.tenantLocked(j.Tenant)
		s.pushReadyLocked(j, tv)
		s.unfit[i] = s.unfit[len(s.unfit)-1]
		s.unfit = s.unfit[:len(s.unfit)-1]
		i--
	}

	// 2) 退避中的重试作业：扫 retryQ 里到期的。
	kept := s.retryQ[:0]
	for _, j := range s.retryQ {
		j.mu.Lock()
		notBefore, state, group := j.NotBefore, j.State, j.Group
		j.mu.Unlock()
		if state != StatePending {
			continue // 已终态，丢弃条目
		}
		if now.Before(notBefore) {
			kept = append(kept, j)
			continue
		}
		// 所属组已被取消：重试毫无意义（组 context 已死），直接落 canceled。
		if group != "" && s.groupCanceledLocked(group) {
			s.cancelCascadeLocked(j, "所属作业组 "+group+" 已取消，放弃重试")
			continue
		}
		tv := s.tenantLocked(j.Tenant)
		s.pushReadyLocked(j, tv)
	}
	s.retryQ = kept
}

// ---- 终态与重试 ----------------------------------------------------------

// Complete 由 runner 在作业执行结束后调用，负责：
//   - 成功：终态 succeeded、释放资源、熔断器复位、唤醒下游、释放资源给 unfit 作业；
//   - 失败：按指数退避重排（未超上限）或终态 failed + 熔断计数。
//
// 该方法是幂等来源之一：running 计数与资源释放都做了去重保护。
func (s *Scheduler) Complete(h jobHandle, runErr error) {
	j := h.j

	// 资源释放必须且只能发生一次（取消传播与正常完成可能同时到达）。
	released := h.lease.Release()

	s.mu.Lock()
	tv := s.tenantLocked(j.Tenant)
	if tv.running > 0 {
		tv.running--
	}
	tv.used = s.pool.TenantUsed(j.Tenant)

	st := j.StateOf()
	now := time.Now()
	j.mu.Lock()
	j.FinishedAt = now
	j.mu.Unlock()

	// 非终态才继续流转；若已被级联取消（StateCanceled），只做资源与统计收尾。
	if st.Terminal() {
		s.mu.Unlock()
		s.trace(TraceEvent{Kind: "cancel", JobID: j.ID, Tenant: j.Tenant,
			Msg: "作业已被取消，忽略其执行结果: " + errText(runErr), Used: s.pool.Used()})
		return
	}

	if runErr == nil {
		// ---- 成功 ----
		_ = j.transition(StateSucceeded)
		j.signalDone()
		tv.breaker.onSuccess()
		_ = s.wal.Append(WLEvent{Event: EvtSucceeded, Job: record(j)})
		s.nCompleted++
		groupID := j.Group
		s.mu.Unlock()
		s.trace(TraceEvent{Kind: "ok", JobID: j.ID, Tenant: j.Tenant,
			Msg:  fmt.Sprintf("成功（第 %d 次尝试，资源已释放=%v）", j.attemptOf(), released),
			Used: s.pool.Used()})
		if groupID != "" {
			s.groupSucceeded(j, groupID)
		}
		// 无论是否属于作业组，终态都必须唤醒下游（下游只认上游终态）。
		s.notifyDependents(j)
		s.releaseUnfit()
		return
	}

	// ---- 失败路径 ----
	j.setError(runErr)

	// 取消类错误：不算业务失败，不触发熔断，直接 canceled。
	if errors.Is(runErr, context.Canceled) {
		_ = j.transition(StateCanceled)
		j.signalDone()
		_ = s.wal.Append(WLEvent{Event: EvtCanceled, Reason: runErr.Error(), Job: record(j)})
		s.nCanceled++
		s.mu.Unlock()
		s.trace(TraceEvent{Kind: "cancel", JobID: j.ID, Tenant: j.Tenant,
			Msg: "执行被取消: " + runErr.Error(), Used: s.pool.Used()})
		// 取消也是终态：下游应据此级联取消（不能继续等一个永远不会成功的上游）。
		s.notifyDependents(j)
		s.releaseUnfit()
		return
	}

	// 所属依赖组已被取消 -> 本作业没有重试意义（组 context 已死，重试必然立刻被取消）。
	// 直接落 canceled，避免白白消耗重试次数。
	if j.Group != "" && s.groupCanceledLocked(j.Group) {
		_ = j.transition(StateCanceled)
		j.signalDone()
		_ = s.wal.Append(WLEvent{Event: EvtCanceled, Reason: "所属作业组 " + j.Group + " 已取消", Job: record(j)})
		s.nCanceled++
		s.mu.Unlock()
		s.trace(TraceEvent{Kind: "cancel", JobID: j.ID, Tenant: j.Tenant,
			Msg: fmt.Sprintf("所属作业组 %s 已取消，放弃重试: %v", j.Group, runErr), Used: s.pool.Used()})
		s.notifyDependents(j)
		return
	}

	// 可重试 -> 指数退避重排为 pending。
	if j.canRetry(runErr) {
		backoff := RetryBackoff(retryBackoffBase, j.attemptOf(), j.Seq)
		_ = j.transition(StatePending)
		j.setNotBefore(now.Add(backoff))
		// 注意：中间失败不计入熔断。熔断衡量的是"作业最终失败"，
		// 若把重试也计入，一个重试后成功的作业会误伤整个租户。
		_ = s.wal.Append(WLEvent{Event: EvtRetry, Reason: runErr.Error(), Job: record(j)})
		// 入重试队列前先摘除旧条目，避免退避期被重复 promote 回堆。
		s.dropRetryLocked(j)
		s.retryQ = append(s.retryQ, j)
		groupID := j.Group
		s.mu.Unlock()
		s.trace(TraceEvent{Kind: "retry", JobID: j.ID, Tenant: j.Tenant,
			Msg:  fmt.Sprintf("失败：%v；第 %d/%d 次，退避 %s 后重试", runErr, j.attemptOf(), j.MaxAttempts, backoff),
			Used: s.pool.Used()})
		// 组内作业进入重试：取消组内兄弟（它们的下游数据已不完整），
		// 但**不**级联取消外部下游——本作业仍可能重试成功。
		if groupID != "" {
			s.cancelGroupSiblings(j, groupID, runErr)
		}
		return
	}

	// ---- 重试耗尽：终态 failed + 熔断计数 ----
	_ = j.transition(StateFailed)
	j.signalDone()
	tv.breaker.onFailure(now)
	_ = s.wal.Append(WLEvent{Event: EvtFailed, Reason: runErr.Error(), Job: record(j)})
	s.nFailed++
	breakerMsg := ""
	if tv.breaker.state == breakerOpen {
		breakerMsg = fmt.Sprintf("；租户 %s 连续失败达 %d 次，熔断 %s", tv.name, tv.breaker.failures, BreakerCooldown)
	}
	groupID := j.Group
	s.mu.Unlock()
	s.trace(TraceEvent{Kind: "fail", JobID: j.ID, Tenant: j.Tenant,
		Msg:  fmt.Sprintf("最终失败（已尝试 %d 次）：%v%s", j.attemptOf(), runErr, breakerMsg),
		Used: s.pool.Used()})
	if groupID != "" {
		s.cancelGroupSiblings(j, groupID, runErr)
	}
	// 终态失败：下游一律级联取消。
	s.notifyDependents(j)
	s.releaseUnfit()
}

func errText(err error) string {
	if err == nil {
		return "无错误"
	}
	return err.Error()
}

// releaseUnfit 在资源释放后唤醒因装不下而暂存的作业。
func (s *Scheduler) releaseUnfit() {
	s.mu.Lock()
	pending := len(s.unfit) > 0 || len(s.retryQ) > 0
	s.mu.Unlock()
	if pending {
		s.wake()
	}
}

// ---- 依赖组与级联取消 ----------------------------------------------------

// groupSucceeded 在依赖组的一个子任务成功后推进组状态。
//
// 唤醒下游由 Complete 统一调用 notifyDependents 完成（无论作业是否属于组），
// 这里只负责组内取消语义，避免"只有带组的作业才能唤醒下游"这种不对称。
func (s *Scheduler) groupSucceeded(j *Job, groupID string) {
	s.mu.Lock()
	alreadyFailed := s.groupCancelReason[groupID]
	s.mu.Unlock()
	if alreadyFailed != "" {
		return // 组已被取消
	}
	s.wake()
}

// notifyDependents 唤醒所有以 j 为上游的作业。
// 上游成功 -> 下游回队列；上游失败/取消 -> 下游立刻级联取消（不空占资源）。
func (s *Scheduler) notifyDependents(up *Job) {
	s.mu.Lock()
	var readyJobs, cancelJobs []*Job
	for id, reasons := range s.waiting {
		depends := false
		for _, dep := range id2deps(id, s) {
			if dep == up.ID {
				depends = true
				break
			}
		}
		if !depends {
			continue
		}
		_ = reasons
		delete(s.waiting, id)
		j, ok := s.byID[id]
		if !ok {
			continue
		}
		upState := up.StateOf()
		if !upState.Terminal() {
			// 上游只是进入重试（仍是非终态）：既不放行也不取消，
			// 重新挂回等待列表，等它真正落到终态再决定。
			s.waiting[id] = append(s.waiting[id], "上游 "+up.ID+" 仍在重试")
			continue
		}
		if upState == StateSucceeded {
			// 重新评估所有依赖（可能还有别的上游没完成）。
			if blocked, _ := s.dependencyBlockedLocked(j); blocked {
				s.waiting[id] = append(s.waiting[id], "仍有上游未完成")
				continue
			}
			readyJobs = append(readyJobs, j)
		} else {
			cancelJobs = append(cancelJobs, j)
		}
	}
	for _, j := range readyJobs {
		tv := s.tenantLocked(j.Tenant)
		s.pushReadyLocked(j, tv)
	}
	s.mu.Unlock()

	for _, j := range cancelJobs {
		s.cancelCascade(j, fmt.Sprintf("上游 %s 终态为 %s", up.ID, up.StateOf()))
	}
}

// id2deps 返回作业的依赖列表（调用方持锁）。
func id2deps(id string, s *Scheduler) []string {
	if j, ok := s.byID[id]; ok {
		return j.DependsOn
	}
	return nil
}

// cancelCascade 在持锁状态下把作业标记为 canceled，并回收其资源/队列位置。
func (s *Scheduler) cancelCascadeLocked(j *Job, reason string) {
	if j.StateOf().Terminal() {
		return
	}
	_ = j.transition(StateCanceled)
	j.setError(errors.New(reason))
	j.signalDone()
	_ = s.wal.Append(WLEvent{Event: EvtCanceled, Reason: reason, Job: record(j)})
	s.nCanceled++
}

// cancelCascade 是 cancelCascadeLocked 的加锁版本，并继续向下游传播。
func (s *Scheduler) cancelCascade(j *Job, reason string) {
	s.mu.Lock()
	s.cancelCascadeLocked(j, reason)
	// 从 unfit / retryQ 中摘除，避免之后又被调度。
	s.removeFromQueuesLocked(j)
	s.mu.Unlock()
	s.trace(TraceEvent{Kind: "cancel", JobID: j.ID, Tenant: j.Tenant, Msg: reason})
	// 继续级联：取消的下游还有它自己的下游。
	s.notifyDependents(j)
	s.wake()
}

// groupCanceledLocked 判断依赖组是否已被取消（调用方须持锁）。
func (s *Scheduler) groupCanceledLocked(groupID string) bool {
	return s.groupCancelReason[groupID] != ""
}

// dropRetryLocked 从重试队列中移除作业的旧条目（防止重复入队导致多次调度）。
func (s *Scheduler) dropRetryLocked(j *Job) {
	out := s.retryQ[:0]
	for _, x := range s.retryQ {
		if x != j {
			out = append(out, x)
		}
	}
	s.retryQ = out
}

// cancelGroupSiblings 在组内某个作业失败（含进入重试）时，取消组 context，
// 让 errgroup 里仍在执行的兄弟作业立刻排空退出并释放资源。
// 只影响同组作业，不向下游传播（下游要等本作业终态）。
//
// 关键：必须同时登记 groupCancelReason，否则 promoteEligibleLocked 会把
// "已随组取消"的作业重新 promote 回队列，白白消耗一次重试（组 context 已死，
// 它一起来就会被立刻取消）。
func (s *Scheduler) cancelGroupSiblings(j *Job, groupID string, cause error) {
	s.mu.Lock()
	if s.groupCancelReason[groupID] == "" {
		s.groupCancelReason[groupID] = cause.Error()
		_ = s.wal.Append(WLEvent{Event: EvtCancelGroup, Group: groupID, Reason: cause.Error()})
	}
	s.mu.Unlock()

	if r := s.runner; r != nil {
		r.CancelGroup(groupID, fmt.Sprintf("同组作业 %s 失败: %v", j.ID, cause))
	}
}

// removeFromQueuesLocked 把作业从所有待调度容器里摘掉。
func (s *Scheduler) removeFromQueuesLocked(j *Job) {
	// ready 堆：惰性删除（pick 时会丢弃非 pending 的堆顶），这里只清标记。
	j.mu.Lock()
	j.ready = false
	j.mu.Unlock()

	filtered := s.unfit[:0]
	for _, x := range s.unfit {
		if x != j {
			filtered = append(filtered, x)
		}
	}
	s.unfit = filtered

	filtered2 := s.retryQ[:0]
	for _, x := range s.retryQ {
		if x != j {
			filtered2 = append(filtered2, x)
		}
	}
	s.retryQ = filtered2
	delete(s.waiting, j.ID)
}

// ---- 状态查询与优雅停机 --------------------------------------------------

// Status 返回调度器的可观测快照。
type SchedulerStatus struct {
	Draining   bool             `json:"draining"`
	Pending    int              `json:"pending"`
	Running    int              `json:"running"`
	Succeeded  int              `json:"succeeded"`
	Failed     int              `json:"failed"`
	Canceled   int              `json:"canceled"`
	Rejected   int              `json:"rejected"`
	Duplicate  int              `json:"duplicate"`
	Unfit      int              `json:"unfit"`
	RetryQueue int              `json:"retry_queue"`
	Used       Resources        `json:"used"`
	Free       Resources        `json:"free"`
	Tenants    []TenantSnapshot `json:"tenants"`
	Groups     []GroupStatus    `json:"groups"`
}

// GroupStatus 是依赖组的可观测状态。
type GroupStatus struct {
	ID     string `json:"id"`
	Cancel string `json:"cancel_reason,omitempty"`
	Closed bool   `json:"closed"`
}

// Status 汇总调度器状态（终端 status 子命令用）。
func (s *Scheduler) Status() SchedulerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SchedulerStatus{
		Draining:   s.draining,
		Pending:    s.ready.Len(),
		Unfit:      len(s.unfit),
		RetryQueue: len(s.retryQ),
		Duplicate:  s.nDuplicate,
		Rejected:   s.nRejected,
		Used:       s.pool.Used(),
		Free:       s.pool.Free(),
		Tenants:    s.pool.Tenants(),
	}
	for _, tv := range s.tenants {
		if tv.running > 0 {
			st.Running += tv.running
		}
	}
	for _, j := range s.byID {
		switch j.StateOf() {
		case StateSucceeded:
			st.Succeeded++
		case StateFailed:
			st.Failed++
		case StateCanceled:
			st.Canceled++
		}
	}
	for id, reason := range s.groupCancelReason {
		st.Groups = append(st.Groups, GroupStatus{ID: id, Cancel: reason, Closed: true})
	}
	sort.Slice(st.Groups, func(a, b int) bool { return st.Groups[a].ID < st.Groups[b].ID })
	return st
}

// Jobs 返回所有作业的快照（按提交序号排序）。
func (s *Scheduler) Jobs() []JobView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]JobView, 0, len(s.byID))
	for _, j := range s.byID {
		out = append(out, j.View())
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Recovered != out[b].Recovered {
			return !out[a].Recovered // 原生作业排前面
		}
		return out[a].ID < out[b].ID
	})
	return out
}

// ReplayRestore 依据 WAL 重放结果恢复在途作业。
// 终态作业不重新调度；在途（pending / running / 退避中）作业回到调度器。
func (s *Scheduler) ReplayRestore(recs []*WALJobRecord, hydrate func(*WALJobRecord) *Job) int {
	restored := 0
	s.mu.Lock()
	for _, rec := range recs {
		j := hydrate(rec)
		if j == nil {
			continue
		}
		j.Recovered = true
		s.byID[j.ID] = j
		if j.IdemKey != "" {
			s.byIdem[j.IdemKey] = j
		}
		if j.Seq > s.seq {
			s.seq = j.Seq
		}
		// 终态作业只恢复状态，不参与调度。
		if j.State.Terminal() {
			j.signalDone()
			j.forceState(j.State)
			continue
		}
		// running 状态的作业在崩溃时已经失去执行体，按 pending 重排（幂等键保证不会双跑）。
		if j.StateOf() == StateRunning {
			j.forceState(StatePending)
		}
		tv := s.tenantLocked(j.Tenant)
		restored++
		// 依赖未满足的先阻塞，等同正常提交路径。
		if blocked, reason := s.dependencyBlockedLocked(j); blocked {
			if isCascade(reason) {
				s.cancelCascadeLocked(j, reason)
				continue
			}
			s.waiting[j.ID] = append(s.waiting[j.ID], reason)
			continue
		}
		s.pushReadyLocked(j, tv)
	}
	s.mu.Unlock()
	s.trace(TraceEvent{Kind: "restore", Msg: fmt.Sprintf("WAL 重放恢复在途作业 %d 个", restored)})
	s.wake()
	return restored
}

// wake 唤醒调度循环做一次新决策（非阻塞，信号量语义）。
func (s *Scheduler) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

// WakeChan 暴露调度唤醒信号，供 Run 循环消费。
func (s *Scheduler) WakeChan() <-chan struct{} { return s.wakeCh }

// BeginDrain 停止接收新提交。
func (s *Scheduler) BeginDrain() {
	s.drainOnce.Do(func() {
		s.mu.Lock()
		s.draining = true
		s.mu.Unlock()
	})
}

// Lookup 按 ID 查找作业（只读快照，可能为 nil）。
func (s *Scheduler) Lookup(id string) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}

// DRFDecision 是调度器对"下一个该跑哪个作业"的一次决策结果。
type DRFDecision struct {
	JobID          string     `json:"job_id,omitempty"`
	Tenant         string     `json:"tenant,omitempty"`
	Dominant       float64    `json:"dominant"`
	Ratios         [3]float64 `json:"ratios"` // cpu/mem/gpu 占比
	PendingCount   int        `json:"pending_count"`
	ScannedTenants int        `json:"scanned_tenants"`
}

// DecideLocked 执行一次完整的 DRF 决策：在所有可调度的 pending 作业中，
// 挑使"最大主导份额"最小化的那一个。
//
// 这是 pickLocked 的无副作用内核（只读），供 bench 测量单次决策耗时，
// 也用于 status 解释"为什么是它先跑"。调用方须持 s.mu。
func (s *Scheduler) DecideLocked() DRFDecision {
	total := s.pool.Total()
	dec := DRFDecision{PendingCount: s.ready.Len(), ScannedTenants: len(s.tenants)}
	if len(s.tenants) == 0 {
		return dec
	}

	// 主导份额必须基于**权威**用量：从 Pool 读取，而不是调度器的缓存视图
	// （缓存只在 pickLocked 时同步，直接查会读到陈旧值）。
	ratios := make(map[string][3]float64, len(s.tenants))
	for name, tv := range s.tenants {
		used := s.pool.TenantUsed(name)
		// 池里没有占用时回退到缓存（覆盖手动预留但尚未同步的极端情况）。
		if used.IsZero() && !tv.used.IsZero() {
			used = tv.used
		}
		c, m, g := 0.0, 0.0, 0.0
		if total.CPU > 0 {
			c = float64(used.CPU) / float64(total.CPU)
		}
		if total.Mem > 0 {
			m = float64(used.Mem) / float64(total.Mem)
		}
		if total.GPU > 0 {
			g = float64(used.GPU) / float64(total.GPU)
		}
		ratios[name] = [3]float64{c, m, g}
	}

	// 选主导份额最小的租户；并列时用该租户最早提交的作业序号做 FIFO 兜底，
	// 避免 map 迭代顺序随机化导致决策不可复现。
	bestName := ""
	bestDominant := 0.0
	bestSeq := uint64(0)
	for name, r := range ratios {
		d := max3(r[0], r[1], r[2])
		seq := s.earliestPendingSeqLocked(name)
		if bestName == "" || d < bestDominant ||
			(d == bestDominant && seq < bestSeq) {
			bestName, bestDominant, bestSeq = name, d, seq
		}
	}
	if bestName == "" {
		return dec
	}
	dec.Tenant = bestName
	dec.Dominant = bestDominant
	dec.Ratios = ratios[bestName]

	// 找到该租户下最早提交的候选作业。
	var bestJob *readyItem
	for _, it := range s.ready {
		if it.tenant != bestName {
			continue
		}
		if bestJob == nil || it.seq < bestJob.seq {
			bestJob = it
		}
	}
	if bestJob != nil {
		dec.JobID = bestJob.job.ID
		dec.Dominant = bestJob.score
		dec.Ratios = [3]float64{bestJob.dCPU, bestJob.dMem, bestJob.dGPU}
	}
	return dec
}

// earliestPendingSeqLocked 返回该租户当前最早的 pending 作业序号（无则返回 ^uint64(0)）。
func (s *Scheduler) earliestPendingSeqLocked(tenant string) uint64 {
	best := ^uint64(0)
	for _, it := range s.ready {
		if it.tenant == tenant && it.seq < best {
			best = it.seq
		}
	}
	return best
}

// Decide 是 DecideLocked 的加锁版本（对外只读查询）。
func (s *Scheduler) Decide() DRFDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.DecideLocked()
}
