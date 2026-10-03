package main

import (
	"container/heap"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 优先队列：按 DRF 分数取最小
// ---------------------------------------------------------------------------

// readyHeap 是就绪作业的小顶堆，堆顶为「DRF 分数最低」即最该被调度的作业。
// 分数最小化是 DRF 的标准形式：在每一轮里挑那个把（所有竞争租户中的）
// 最大主导份额压到最小的作业。
type readyHeap struct {
	items []*Job
}

func (h readyHeap) Len() int { return len(h.items) }
func (h readyHeap) Less(i, j int) bool {
	// 分数相同则用全局序号做 FIFO 兜底，保证调度可复现、不抖动。
	if h.items[i].score != h.items[j].score {
		return h.items[i].score < h.items[j].score
	}
	return h.items[i].Seq < h.items[j].Seq
}
func (h readyHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].scoreSeq, h.items[j].scoreSeq = h.items[j].scoreSeq, h.items[i].scoreSeq
}

func (h *readyHeap) Push(x any) {
	j := x.(*Job)
	j.scoreSeq = -1 // -1 标记「本轮刚打分」
	h.items = append(h.items, j)
}

func (h *readyHeap) Pop() any {
	old := h.items
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	h.items = old[:n-1]
	return it
}

// ---------------------------------------------------------------------------
// 同键互斥
// ---------------------------------------------------------------------------

// keyedMutex 保证「同一幂等键」不会有两个执行体同时在跑。幂等提交只在入队
// 时去重一次，之后作业可能被重投、也可能被 WAL 恢复出第二份；真正的防双跑
// 闸门在这里，执行期兜底。
type keyedMutex struct {
	mu    sync.Mutex
	held  map[string]int // key -> 持有者计数
	waits int64          // 曾发生互斥等待的次数
}

func newKeyedMutex() *keyedMutex { return &keyedMutex{held: make(map[string]int)} }

// acquire 尝试拿到 key。返回的 release 函数必须被调用，否则会永久占住。
func (m *keyedMutex) acquire(key string) (release func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := m.held[key]; n > 0 {
		atomic.AddInt64(&m.waits, 1)
		return nil, false
	}
	m.held[key] = 1
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if n := m.held[key]; n <= 1 {
			delete(m.held, key)
		} else {
			m.held[key] = n - 1
		}
	}, true
}

func (m *keyedMutex) Waits() int64 { return atomic.LoadInt64(&m.waits) }

func (m *keyedMutex) Held() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.held)
}

// ---------------------------------------------------------------------------
// 熔断器
// ---------------------------------------------------------------------------

// 熔断阈值与半开放探测，按需求固定为「连续失败 5 次熔断 60 秒」。
const (
	breakerFailureThreshold = 5
	breakerCooldown         = 60 * time.Second
	breakerHalfOpenProbes   = 1
)

type breakerState int

const (
	breakerClosed   breakerState = iota // 正常放行
	breakerOpen                         // 熔断中，拒发
	breakerHalfOpen                     // 半开放，放行少量探测
)

func (s breakerState) String() string {
	switch s {
	case breakerClosed:
		return "closed"
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half-open"
	}
	return "?"
}

// breakerOpenUntil 返回熔断结束时刻；不在熔断中返回零值。
func (b *tenantBreaker) breakerOpenUntil() (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != breakerOpen {
		return time.Time{}, false
	}
	return b.openUntil, true
}

type tenantBreaker struct {
	mu sync.Mutex

	// cooldown 是熔断持续时长，生产固定 60s；测试可缩短。
	cooldown time.Duration

	state     breakerState
	consec    int
	openUntil time.Time
	// inFlight 是当前半开放期已放行的探测数。
	inFlight int
	// probeResult 记录本次半开放期探测的结果，为真则熔断器彻底关闭。
	probeResult bool
	probeSeen   bool

	// 统计
	trips  int
	probes int
	lastAt time.Time
}

func newTenantBreaker(cooldown time.Duration) *tenantBreaker {
	if cooldown <= 0 {
		cooldown = breakerCooldown
	}
	return &tenantBreaker{state: breakerClosed, cooldown: cooldown}
}

// Allow 决定一个作业能否放行，并处理熔断到期 → 半开放的跃迁。
func (b *tenantBreaker) Allow(now time.Time) (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case breakerOpen:
		if now.Before(b.openUntil) {
			return false, fmt.Sprintf("circuit open until %s (%s remaining)",
				b.openUntil.Format(time.RFC3339), b.openUntil.Sub(now).Round(time.Millisecond))
		}
		// 冷却结束：进入半开放，只放一个探测作业探活。
		b.state = breakerHalfOpen
		b.inFlight = 0
		b.probeSeen = false
	case breakerHalfOpen:
		if b.probeSeen {
			return false, "circuit half-open: probe already in flight"
		}
	}
	if b.state == breakerHalfOpen {
		if b.inFlight >= breakerHalfOpenProbes {
			return false, "circuit half-open: probe budget exhausted"
		}
		b.inFlight++
		b.probes++
	}
	return true, ""
}

// Record 回报一次执行结果，维护连续失败计数与熔断状态机。
// canceled 不计入连续失败——上游取消不是租户的错。
func (b *tenantBreaker) Record(ok bool, canceled bool, now time.Time) (tripped bool, state breakerState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAt = now

	if canceled {
		// 半开放期的取消不算探活成功，让它退回 open 等下一个冷却窗口。
		if b.state == breakerHalfOpen && !b.probeSeen {
			b.state = breakerOpen
			b.openUntil = now.Add(b.cooldown)
			b.inFlight = 0
		}
		return false, b.state
	}

	if b.state == breakerHalfOpen {
		b.probeSeen = true
		if b.inFlight > 0 {
			b.inFlight--
		}
		if ok {
			b.state = breakerClosed
			b.consec = 0
		} else {
			b.state = breakerOpen
			b.openUntil = now.Add(b.cooldown)
			b.trips++
			tripped = true
		}
		return tripped, b.state
	}

	if ok {
		b.consec = 0
		return false, b.state
	}
	b.consec++
	if b.consec >= breakerFailureThreshold && b.state == breakerClosed {
		b.state = breakerOpen
		b.openUntil = now.Add(b.cooldown)
		b.trips++
		tripped = true
	}
	return tripped, b.state
}

// ForceOpen 直接把某租户打到熔断，用于演示与运维。
func (b *tenantBreaker) ForceOpen(now time.Time) {
	b.mu.Lock()
	b.state = breakerOpen
	b.openUntil = now.Add(b.cooldown)
	b.consec = breakerFailureThreshold
	b.mu.Unlock()
}

type breakerView struct {
	State     string    `json:"state"`
	Consec    int       `json:"consecutive_failures"`
	OpenUntil time.Time `json:"open_until,omitempty"`
	Trips     int       `json:"trips"`
	Probes    int       `json:"probes"`
	OpenLeft  string    `json:"open_remaining,omitempty"`
}

func (b *tenantBreaker) View(now time.Time) breakerView {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := breakerView{State: b.state.String(), Consec: b.consec, Trips: b.trips, Probes: b.probes}
	if b.state == breakerOpen {
		v.OpenUntil = b.openUntil
		if d := b.openUntil.Sub(now); d > 0 {
			v.OpenLeft = d.Round(time.Millisecond).String()
		}
	}
	return v
}

func (b *tenantBreaker) String() string {
	v := b.View(time.Now())
	s := fmt.Sprintf("%s(consec=%d trips=%d)", v.State, v.Consec, v.Trips)
	if v.OpenLeft != "" {
		s += fmt.Sprintf(" reset_in=%s", v.OpenLeft)
	}
	return s
}

// ---------------------------------------------------------------------------
// DRF 主导份额
// ---------------------------------------------------------------------------

// dominantShare 返回某租户在三维上的主导份额（各维度占全池比例的最大值）。
// 这是 DRF 的核心量：谁的维度吃得最狠，谁就是当前的「份额大户」。
//
// 请求为零的维度不参与取最大——一个只要 2 核内存的轻量评测，不应该因为
// 内存维度是 0 而被算成 0 份额。零需求维度对份额没有约束力，忽略它们。
func dominantShare(used, total Resources) float64 {
	m := 0.0
	if total.CPU > 0 && used.CPU > 0 {
		m = math.Max(m, float64(used.CPU)/float64(total.CPU))
	}
	if total.MemMB > 0 && used.MemMB > 0 {
		m = math.Max(m, float64(used.MemMB)/float64(total.MemMB))
	}
	if total.GPU > 0 && used.GPU > 0 {
		m = math.Max(m, float64(used.GPU)/float64(total.GPU))
	}
	return m
}

// tenantShare 是某租户当前的 DRF 描述。
type tenantShare struct {
	Tenant    string    `json:"tenant"`
	Usage     Resources `json:"usage"`
	Share     float64   `json:"dominant_share"`
	Pending   int       `json:"pending_jobs"`
	Running   int       `json:"running_jobs"`
	Known     bool      `json:"known"` // 是否已经进入过调度视野
	ColdStart bool      `json:"cold"`  // 刚进视野、还没有历史份额
}

func (t tenantShare) String() string {
	return fmt.Sprintf("%s share=%.4f use=%s pend=%d run=%d", t.Tenant, t.Share, t.Usage, t.Pending, t.Running)
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

type schedulerOptions struct {
	// fairnessWeight 控制饥饿老化：等待每 waitQuantum 加一个权重点。
	fairnessWeight float64
	// waitQuantum 是老化计时的量子。
	waitQuantum time.Duration
	// breakerCooldown 允许测试缩短（生产固定 60s）。
	breakerCooldown time.Duration
}

func defaultSchedulerOptions() schedulerOptions {
	return schedulerOptions{
		fairnessWeight:  1.0,
		waitQuantum:     2 * time.Second,
		breakerCooldown: breakerCooldown,
	}
}

func (o schedulerOptions) withDefaults() schedulerOptions {
	d := defaultSchedulerOptions()
	if o.fairnessWeight <= 0 {
		o.fairnessWeight = d.fairnessWeight
	}
	if o.waitQuantum <= 0 {
		o.waitQuantum = d.waitQuantum
	}
	if o.breakerCooldown <= 0 {
		o.breakerCooldown = d.breakerCooldown
	}
	return o
}

// Scheduler 持有队列、租户账本与决策逻辑。它被设计成「逻辑全部串行、查询
// 全部并发安全」：Submit 由主循环串行调用（去重需要），Snapshot/Stats
// 可以从任意 goroutine 调用。
type Scheduler struct {
	mu   sync.Mutex
	opts schedulerOptions
	// logger 可选，仅用于输出记账异常。
	logger Logger

	ready readyHeap
	// waiting 是「依赖未满足 / 退避中」的作业，按 wake 时刻排期。
	waiting []*Job

	index    map[string]*Job // 幂等键 -> 作业（活跃集合）
	byTenant map[string]*tenantState
	seq      int64

	// terminalMem 记住已终结作业的终态。作业一旦终结就从 index 移除，
	// 但下游作业的依赖判定还需要知道「上游到底是成功还是失败」——
	// 否则上游一失败就消失，下游会永远等下去。
	terminalMem map[string]JobState

	// draining 为 true 时 Select 一律拒绝派发（优雅停机第一阶段）。
	draining bool

	breakers map[string]*tenantBreaker
	// knownTenants 记录历史上出现过的租户，用于冷启动补偿：
	// 新出现的租户不是「份额 0 的陌生人」，而是「曾经用过池的老朋友」。
	knownTenants map[string]bool
	// warmShares 是每个已知租户最后一次被观测到的主导份额。
	warmShares map[string]float64

	km *keyedMutex

	// breakerLogAt 记录每个租户上次打印熔断日志的时刻，用于限流：
	// 熔断期间每个决策周期都会撞上 Allow 失败，不限流会把轨迹刷屏。
	breakerLogAt map[string]time.Time

	// ---- 统计 ----
	decisions     atomic.Int64
	rejectedCont  atomic.Int64
	rejectedBreak atomic.Int64
	starved       atomic.Int64
}

// tenantState 是单租户的调度侧状态。
type tenantState struct {
	name    string
	pending int
	running int
	// reserved 统计「已 dispatch 但退避/未释放」的作业，重放时用它对齐份额。
	reserved Resources
}

// SetLogger 给调度器装上日志出口。
func (s *Scheduler) SetLogger(l Logger) {
	s.mu.Lock()
	s.logger = l
	s.mu.Unlock()
}

func NewScheduler(opts schedulerOptions) *Scheduler {
	s := &Scheduler{
		opts:         opts.withDefaults(),
		index:        make(map[string]*Job),
		byTenant:     make(map[string]*tenantState),
		terminalMem:  make(map[string]JobState),
		breakerLogAt: make(map[string]time.Time),
		breakers:     make(map[string]*tenantBreaker),
		knownTenants: make(map[string]bool),
		warmShares:   make(map[string]float64),
		km:           newKeyedMutex(),
	}
	heap.Init(&s.ready)
	return s
}

func (s *Scheduler) Breaker(tenant string) *tenantBreaker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.breakerLocked(tenant)
}

func (s *Scheduler) breakerLocked(tenant string) *tenantBreaker {
	b, ok := s.breakers[tenant]
	if !ok {
		b = newTenantBreaker(s.opts.breakerCooldown)
		s.breakers[tenant] = b
	}
	return b
}

func (s *Scheduler) tenantLocked(tenant string) *tenantState {
	t, ok := s.byTenant[tenant]
	if !ok {
		t = &tenantState{name: tenant}
		s.byTenant[tenant] = t
	}
	return t
}

// ---------------------------------------------------------------------------
// 入队 / 去重
// ---------------------------------------------------------------------------

// submitResult 是提交结果。
type submitResult struct {
	Job     *Job
	Created bool // false 表示命中幂等，返回的是已有句柄
	Reason  string
}

// Submit 把作业入队。相同幂等键的重复提交只入队一次，第二次直接返回既有
// 句柄——这就是「手抖重复提交」的第一道闸门。
func (s *Scheduler) Submit(req SubmitRequest) (*Job, bool, error) {
	return s.submit(req, nil)
}

// submitWithCancels 与 Submit 相同，但会把「在提交阶段就被判定取消」的作业
// 通过 onCancel 回调交还给引擎，以便落 WAL。
func (s *Scheduler) submitWithCancels(req SubmitRequest, onCancel func(*Job, string)) (*Job, bool, error) {
	return s.submit(req, onCancel)
}

func (s *Scheduler) submit(req SubmitRequest, onCancel func(*Job, string)) (*Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !req.Force {
		if prev, ok := s.index[req.IdemKey]; ok {
			if prev.Tenant != req.Tenant {
				return prev, false, fmt.Errorf("idempotency key %q already used by tenant %q (owner mismatch)",
					req.IdemKey, prev.Tenant)
			}
			return prev, false, nil
		}
	}

	deps := req.DepRefs
	if len(deps) == 0 && len(req.Deps) > 0 {
		deps = make([]DepRef, 0, len(req.Deps))
		for _, d := range req.Deps {
			deps = append(deps, DepRef{IdemKey: d})
		}
	}
	job := newJob(s.seq, req.Tenant, req.IdemKey, req.Want, deps, req.Work, req.MaxRetries)
	job.Group = req.Group
	job.Name = req.Name
	s.seq++
	job.scoreSeq = s.seq

	s.index[job.IdemKey] = job
	t := s.tenantLocked(job.Tenant)
	t.pending++

	// 依赖判定。必须在这里同步做一次，而不是无脑丢进 waiting：
	// 如果上游已经失败/取消（提交顺序反了，或者上游跑得极快在下一次
	// resolveDependencies 之前就终结了），这个作业一入队就是一个「已经
	// 注定要取消」的僵尸——它会占着 pending 计数直到下一轮依赖扫描才被清理，
	// 在这期间 DRF 会把它当成正常候选，反复重排一个永远不会执行的作业。
	ready, dead, why := s.depsResolvedLocked(job, time.Now())
	switch {
	case dead:
		s.finishLocked(job, StateCanceled, errors.New(why))
		if onCancel != nil {
			onCancel(job, why)
		}
		return job, true, nil
	case len(job.depRefs()) == 0 || ready:
		job.markReady()
		heap.Push(&s.ready, job)
	default:
		// 上游还没出现或还在跑：进 waiting 等唤醒。
		s.waiting = append(s.waiting, job)
	}
	// 新租户冷启动：第一次见到，记录基线但不预热（预热=0，等价于
	// 「你还没用过池」，这就是冷启动补偿的定义）。
	s.knownTenants[job.Tenant] = true
	return job, true, nil
}

// Lookup 按幂等键查活跃作业。
func (s *Scheduler) Lookup(idemKey string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.index[idemKey]
	return j, ok
}

// ---------------------------------------------------------------------------
// DRF 打分与选择
// ---------------------------------------------------------------------------

// score 返回作业的调度分数：候选作业带来的「主导份额」。
//
// 三步：
//  1. 租户当前主导份额 t；
//  2. 放入该作业后的主导份额 t'；
//  3. 取 max(t, t')——这一项专门压制「独占型大作业」。
//
// 第 3 步是本实现与教科书 DRF 的一处偏离，且是刻意的：纯 DRF 只看 t'，
// 于是一个空闲租户即使 post 后主导份额已经超过所有其他租户，只要 t' 仍是
// 全场最小就会被立刻放行。CV 团队那个「一占半天」的 8 卡训练正是这样
// 反复插到评测前面。max(t, t') 相当于一个温和的「份额不能突然暴涨」的
// 软约束：抢跑会让分数超过竞争对手，从而被推迟到真正需要它的时候。
//
// 三个边界情况在下面都有对应处理：零请求维度不参与 max（见 dominantShare）、
// 新租户用历史预热份额而不是 0（见 warmShareLocked）、
// 饥饿老化通过 fairness 权重抬升长期等待的作业。
func (s *Scheduler) score(job *Job, total Resources, now time.Time) float64 {
	// 触碰一次以确保租户状态存在（冷启动时创建）。
	s.tenantLocked(job.Tenant)
	usage := s.usageLocked(job.Tenant)

	tShare := s.warmShareLocked(job.Tenant, usage, total)
	tPrime := dominantShare(addRes(usage, job.Want), total)

	score := math.Max(tShare, tPrime)

	// 饥饿老化：等得越久，分数越低（越容易被选中）。
	// weight = fairnessWeight * (wait / waitQuantum)，2s 等待记 1 分，
	// 足以抵消任何维度的份额差，从而保证长期等待者最终被排上。
	if s.opts.fairnessWeight > 0 {
		wait := job.waitFor(now)
		score -= s.opts.fairnessWeight * (float64(wait) / float64(s.opts.waitQuantum))
	}
	return score
}

// warmShareLocked 取租户参与比较用的基线份额。
// 对已经跑过东西的老租户，用其历史峰值份额预热，避免它在资源刚释放、
// 自身份额已回落到 0 的瞬间被误判成「最闲的租户」而连抢几轮；
// 对全新租户，基线就是 0（冷启动补偿：给机会，但不预支份额）。
func (s *Scheduler) warmShareLocked(tenant string, usage Resources, total Resources) float64 {
	cur := dominantShare(usage, total)
	if _, seen := s.warmShares[tenant]; !seen && s.knownTenants[tenant] {
		s.warmShares[tenant] = cur
		return cur
	}
	warm := s.warmShares[tenant]
	if warm > cur {
		// 记忆会缓慢衰减，避免一个曾占满池的租户永久背负高基线。
		s.warmShares[tenant] = warm*0.9 + cur*0.1
		warm = s.warmShares[tenant]
	} else {
		s.warmShares[tenant] = cur
	}
	return warm
}

// TenantReserved 返回某租户当前「已派发未归还」的保留资源，
// 供 WAL 快照与观测使用。
func (s *Scheduler) TenantReserved(tenant string) Resources {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usageLocked(tenant)
}

// submitRecovering 是 WAL 重放专用的入队路径：跳过「提交阶段就判定依赖已死」
// 的即时取消逻辑。恢复时终态记忆表是空的，作业之间的依赖要等
// resolveDependencies 按重建出来的顺序重新判定，这里提前取消会误杀。
func (s *Scheduler) submitRecovering(req SubmitRequest) (*Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !req.Force {
		if prev, ok := s.index[req.IdemKey]; ok {
			return prev, false, nil
		}
	}
	deps := req.DepRefs
	job := newJob(s.seq, req.Tenant, req.IdemKey, req.Want, deps, req.Work, req.MaxRetries)
	job.Group = req.Group
	job.Name = req.Name
	job.Recovered = true
	s.seq++
	s.index[job.IdemKey] = job
	t := s.tenantLocked(job.Tenant)
	t.pending++
	if len(deps) > 0 {
		s.waiting = append(s.waiting, job)
	} else {
		job.markReady()
		heap.Push(&s.ready, job)
	}
	s.knownTenants[job.Tenant] = true
	return job, true, nil
}

// SeedTenantWarm 只预热 DRF 的打分基线，不碰占用账本 reserved。
func (s *Scheduler) SeedTenantWarm(tenant string, usage Resources) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenantLocked(tenant)
	s.knownTenants[tenant] = true
	total := defaultPoolTotalHint()
	share := dominantShare(usage, total)
	if cur, ok := s.warmShares[tenant]; !ok || share > cur {
		s.warmShares[tenant] = share
	}
}

// defaultPoolTotalHint 只用于恢复期播种基线：它只是给「历史份额」一个量纲，
// 取一个中性的大值即可，真实份额会在首次 Plan 时用实际池容量重算。
func defaultPoolTotalHint() Resources {
	return Resources{CPU: 1024, MemMB: 1024 * 1024, GPU: 256}
}

// SeedTenant 在 WAL 重放时预热某租户的份额基线，
// 让重启后不会因为所有租户份额归零而集体抢跑。
func (s *Scheduler) SeedTenant(tenant string, usage Resources) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tenantLocked(tenant)
	if t.reserved.CPU < usage.CPU {
		t.reserved.CPU = usage.CPU
	}
	if t.reserved.MemMB < usage.MemMB {
		t.reserved.MemMB = usage.MemMB
	}
	if t.reserved.GPU < usage.GPU {
		t.reserved.GPU = usage.GPU
	}
	s.knownTenants[tenant] = true
}

// RememberTerminal 灌入一个已终结作业的终态，供 WAL 重放后下游依赖判定使用。
func (s *Scheduler) RememberTerminal(idemKey string, st JobState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminalMem[idemKey] = st
}

// noTenantReservations 报告是否所有租户的 reserved 都已归零。
// 供测试断言「调度器账本与资源池账本最终对齐」。
func (s *Scheduler) noTenantReservations() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.byTenant {
		if t.reserved != (Resources{}) {
			return false
		}
	}
	return true
}

// ResolveDependencies 是 main 调用 resolveDependencies 的公开包装。
func (s *Scheduler) ResolveDependencies() []*Job {
	return s.resolveDependencies(false)
}

func (s *Scheduler) usageLocked(tenant string) Resources {
	t, ok := s.byTenant[tenant]
	if !ok {
		return Resources{}
	}
	return t.reserved
}

// plan 是 Select 的产物。
type plan struct {
	Job    *Job
	Score  float64
	Before float64
	After  float64
}

// selectErrors 说明这次选作业失败的原因。
type selectErrors struct {
	Drained     bool     // 停机中，不再调度
	NoJobs      bool     // 没有可调度作业
	NoFit       bool     // 有作业但当前资源不足
	Breakers    []string // 被熔断挡住的租户
	BlockedDeps bool     // 全部卡在未满足的依赖上
}

// Plan 选出「下一个该调度」的作业。它只读调度器状态，不碰资源池。
//
// 关键约束：Plan 全程只在调度器锁内运行，绝不调用任何需要资源池锁的方法。
// 之前 Select 是一边持调度器锁一边扣资源，而运行中作业的释放路径是
// 「先查调度器拿 tenant、再锁资源池」，两个方向构成经典的 AB-BA 死锁。
// 现在把两者拆成 Plan / Acquire 两步，彻底消除锁序反转。
func (s *Scheduler) Plan(pool *Pool, now time.Time) (*plan, selectErrors) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var se selectErrors
	if s.draining {
		se.Drained = true
		return nil, se
	}

	available := pool.Available()
	total := pool.Total()

	// 堆里的顺序可能已经过时（别的作业被调度走、份额变了），所以每轮都
	// 重算堆顶候选的分数并排序，而不是信任旧堆序。只看前 candidateDepth
	// 个即可：排在第 12 名之后的作业这一轮本来就轮不到。
	const candidateDepth = 12
	n := s.ready.Len()
	if n > candidateDepth {
		n = candidateDepth
	}
	if n == 0 {
		se.BlockedDeps = len(s.waiting) > 0
		se.NoJobs = se.BlockedDeps
		return nil, se
	}

	type scoredCand struct {
		job   *Job
		score float64
	}
	cands := make([]scoredCand, 0, n)
	for i := 0; i < n; i++ {
		j := s.ready.items[i]
		sc := s.score(j, total, now)
		j.score = sc
		cands = append(cands, scoredCand{job: j, score: sc})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score < cands[j].score
		}
		return cands[i].job.Seq < cands[j].job.Seq
	})

	for _, c := range cands {
		job := c.job
		if canceled, why := job.isCanceled(); canceled {
			s.finishLocked(job, StateCanceled, errors.New(why))
			continue
		}
		if ok, reason := s.breakerLocked(job.Tenant).Allow(now); !ok {
			s.rejectedBreak.Add(1)
			if s.shouldLogBreaker(job.Tenant, now) {
				se.Breakers = append(se.Breakers, fmt.Sprintf("%s: %s", job.Tenant, reason))
			}
			continue
		}
		if !job.Want.Fits(available) {
			// 这个作业此刻塞不下，但排后面的请求更小、可能塞得下，继续看。
			s.rejectedCont.Add(1)
			se.NoFit = true
			continue
		}
		// 进入临界区前先把候选标记为「决策中」，防止并发 Plan 选中同一个。
		if !job.beginPlan() {
			continue
		}
		usage := s.usageLocked(job.Tenant)
		p := &plan{
			Job:    job,
			Score:  c.score,
			Before: dominantShare(usage, total),
			After:  dominantShare(addRes(usage, job.Want), total),
		}
		return p, se
	}

	se.BlockedDeps = len(s.waiting) > 0 && !se.NoFit
	if len(se.Breakers) == 0 && !se.NoFit {
		se.NoJobs = true
	}
	return nil, se
}

// Acquire 尝试为计划中的作业扣减资源。必须在 Plan 之外调用。
//
// 返回的 plan 有效表示扣减成功且作业已转为 running；返回 nil 表示扣减
// 失败（池已满），调用方应当把作业放回队列并稍后重试。
func (s *Scheduler) Acquire(pool *Pool, p *plan) *plan {
	if p == nil {
		return nil
	}
	job := p.Job

	err := pool.Acquire(job.Tenant, job.IdemKey, job.Want)
	if err != nil {
		// 扣减失败：撤销「决策中」标记，作业原样留在堆里等下一轮。
		job.abortPlan()
		if !errors.Is(err, ErrInsufficient) {
			// 请求超过池容量这类永久性错误：直接判失败，避免它永远堵在堆顶。
			s.mu.Lock()
			s.finishLocked(job, StateFailed, err)
			s.mu.Unlock()
		}
		return nil
	}

	// 扣减成功：在调度器锁内完成状态机与计数的收尾。
	s.mu.Lock()
	if job.State().Terminal() { // 理论上不可达：Plan 时已排除终态作业
		s.mu.Unlock()
		pool.Release(job.Tenant, job.IdemKey, job.Want)
		return nil
	}
	if err := job.transition(StateRunning, "dispatched"); err != nil {
		s.mu.Unlock()
		pool.Release(job.Tenant, job.IdemKey, job.Want)
		job.abortPlan()
		return nil
	}
	s.popReady(job)
	// 关键：清掉「决策中」标记，让作业将来重试时还能再次被 Plan 选中。
	// 漏掉这一行会导致 beginPlan 永远返回 false —— 作业第一次派发之后就
	// 再也拿不到第二次调度机会，所有重试都会静默卡死在就绪队列里。
	job.abortPlan()
	t := s.tenantLocked(job.Tenant)
	t.pending--
	t.running++
	t.reserved = addRes(t.reserved, job.Want)
	s.decisions.Add(1)
	s.mu.Unlock()

	return p
}

// RequeueAfterFail 把「扣减失败」的作业放回就绪堆。
func (s *Scheduler) RequeueAfterFail(job *Job) {
	s.mu.Lock()
	// 堆里已经有它（Acquire 失败时没出堆），只需重置老化计时即可。
	job.markReady()
	s.mu.Unlock()
}

// shouldLogBreaker 对熔断日志限流：每个租户最多每 500ms 打一条，
// 保证「熔断确实挡住了作业」在轨迹里可见，又不至于淹没其他事件。
func (s *Scheduler) shouldLogBreaker(tenant string, now time.Time) bool {
	last, ok := s.breakerLogAt[tenant]
	if ok && now.Sub(last) < 500*time.Millisecond {
		return false
	}
	s.breakerLogAt[tenant] = now
	return true
}

func (s *Scheduler) lenWaitingLocked() int { return len(s.waiting) }

// popReady 把作业从就绪堆里摘掉。返回是否真的在堆里。
//
// 这里不能用「把末位元素搬到 i 再 heap.Fix(i)」的写法：当被摘掉的就是
// 末位元素时，它已经不在堆内了，此时 Fix 会拿越界下标去比较 Less，
// 直接 panic。另外一个坑是终态作业必须真正从堆里消失——否则它会一直
// 留在堆顶被反复选中，每次都因为状态机不允许 terminal->running 而
// 白白失败重排队。
func (s *Scheduler) popReady(job *Job) bool {
	for i, j := range s.ready.items {
		if j != job {
			continue
		}
		// heap.Remove 内部是 Swap(0, n-1) + 缩短 + down(0, n-1)，是正确的。
		// 千万别手写成「末位搬到 i 再 Fix(i)」：当被摘的就是末位元素时，
		// 它已经离开堆了，Fix 会拿越界下标去比较 Less 而 panic。
		heap.Remove(&s.ready, i)
		return true
	}
	return false
}

// Requeue 把作业放回就绪堆（重试退避结束后 / 恢复时用）。
//
// 关键是要把状态机从 running 拨回 pending。少了这一步，作业虽然在堆里，
// 但 Plan 里的 Acquire 会因为「running -> running」之外的状态判定失败而
// 一直派发不出去，表现为「RETRY 日志打出来了，之后就再也没动静」——
// 重试静默卡死。
func (s *Scheduler) Requeue(job *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job.State().Terminal() {
		return // 已被取消或已终结，不必再排
	}
	if job.State() == StateRunning {
		if err := job.transition(StatePending, "requeue after backoff"); err != nil {
			return
		}
	}
	// 重新进入就绪堆前先确保不会重复入堆。
	if s.inReady(job) {
		job.markReady()
		return
	}
	job.markReady()
	heap.Push(&s.ready, job)
}

// inReady 判断作业是否已经在就绪堆里。
func (s *Scheduler) inReady(job *Job) bool {
	for _, j := range s.ready.items {
		if j == job {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 结束一个作业
// ---------------------------------------------------------------------------

// Finish 是作业离开系统的唯一出口，负责状态机、租户计数、索引清理三件事。
func (s *Scheduler) Finish(job *Job, st JobState, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishLocked(job, st, cause)
}

// finishLocked 是作业离开系统的唯一记账点。
//
// 注意开头不能写「已终结就直接 return」：执行体（WorkFunc）自己会把作业
// 推到 succeeded，此时状态机已经是终态，但调度器侧的租户账本（reserved /
// running）还完全没动。这里必须继续走完记账，再把「记账过没有」用
// job.accounted 标记住，从而同时满足两个要求：
//   - 状态机不被二次跃迁；
//   - 租户份额与资源池账本一定对齐。
func (s *Scheduler) finishLocked(job *Job, st JobState, cause error) {
	if job.accounted {
		return // 幂等：取消路径可能重复触发
	}
	if cause != nil {
		job.setLastError(cause)
	}
	if !job.State().Terminal() {
		if err := job.transition(st, "finish"); err != nil {
			// 状态机不认这个跃迁：仍然继续记账，保证账本不会漏。
			s.logSkipLocked(job, err)
		}
	}
	job.markAccounted()
	t := s.tenantLocked(job.Tenant)
	if t.running > 0 {
		t.running--
	}
	// 终态作业必须真正离开就绪堆，否则它会留在堆里被反复选中。
	s.popReady(job)
	t.reserved = subRes(t.reserved, job.Want)
	clampRes(&t.reserved)
	delete(s.index, job.IdemKey)
	s.terminalMem[job.IdemKey] = job.State()
	job.released = true
}

// logSkipLocked 记录一次被跳过/异常的终态处理，便于排查账本不一致。
func (s *Scheduler) logSkipLocked(job *Job, err error) {
	if s.logger != nil {
		s.logger("WARN settle %s: %v", job.IdemKey, err)
	}
}

// NoteRetry 在作业退避重试期间把它从 running 退回 pending。
//
// 关键点：必须同时把 reserved 减掉。执行体返回错误时已经把资源还给了资源池
// （runner 的 cleanup），所以调度器侧的「已保留资源」如果不跟着减，重试一次
// 就凭空多记一份，重试 N 次后该租户的 reserved 会涨到池容量的 N 倍——
// 表现就是份额 2.0、3.0 的超卖，以及资源永远放不出来。
//
// 三条记账不变式（任何改动都要维持）：
//
//	pending  = 在就绪堆/等待依赖里的作业数
//	running  = 已派发但未终结的作业数
//	reserved = 已派发作业当前真正占住池子的资源量（必须与 Pool.Allocated 对齐）
func (s *Scheduler) NoteRetry(job *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tenantLocked(job.Tenant)
	if t.running > 0 {
		t.running--
	}
	t.pending++
	t.reserved = subRes(t.reserved, job.Want)
	clampRes(&t.reserved)
}

// clampRes 把可能出现的负残差夹回 0，避免整数记账的舍入误差累积成负份额。
func clampRes(r *Resources) {
	if r.CPU < 0 {
		r.CPU = 0
	}
	if r.MemMB < 0 {
		r.MemMB = 0
	}
	if r.GPU < 0 {
		r.GPU = 0
	}
}

// ---------------------------------------------------------------------------
// 依赖
// ---------------------------------------------------------------------------

// resolveDependencies 把上游已经成功、或者上游已死（要连带取消）的作业
// 从 waiting 里捞出来处理。返回本轮发生状态变化的作业，供 WAL 回写。
func (s *Scheduler) resolveDependencies(canceled bool) []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()

	var changed []*Job
	now := time.Now()
	// 不要用 s.waiting[:0] 原地复用底层数组：循环正在遍历 s.waiting，
	// 往同一个数组里写会把后面还没遍历到的元素覆盖掉，导致某些依赖作业
	// 被静默跳过（曾经表现为 3 个下游只取消了 2 个）。
	// 代价是一次小分配，换来正确性。
	remaining := make([]*Job, 0, len(s.waiting))
	for _, job := range s.waiting {
		ready, dead, why := s.depsResolvedLocked(job, now)
		switch {
		case ready:
			heap.Push(&s.ready, job)
			changed = append(changed, job)
		case dead:
			// 上游死了，下游必须跟着取消——这就是「上游失败后下游不取消
			// 一直占着资源」的修复点：不等资源回收，直接终结并记 WAL。
			s.finishLocked(job, StateCanceled, errors.New(why))
			changed = append(changed, job)
		default:
			remaining = append(remaining, job)
		}
	}
	s.waiting = remaining
	return changed
}

func (s *Scheduler) depsResolvedLocked(job *Job, now time.Time) (ready, dead bool, why string) {
	for _, d := range job.DepRefs {
		up, ok := s.index[d.IdemKey]
		if !ok {
			// 找不到上游：可能是它还没提交（顺序反了），也可能它已终结。
			// 用终态记忆表判断，两种情况分别处理。
			st, known := s.terminalState(d.IdemKey)
			if !known {
				return false, false, "" // 上游还没出现，等着
			}
			if st == StateSucceeded {
				continue
			}
			return false, true, fmt.Sprintf("upstream %s ended as %s", d.IdemKey, st)
		}
		switch up.State() {
		case StatePending, StateRunning:
			// 上游还在跑或还没派发：继续等。这是最容易写错的一处——
			// 若把 pending 归进 default 分支，下游会在上游刚入队、
			// 尚未获得资源时就被判为「上游已死」并连带取消。
			return false, false, ""
		case StateSucceeded:
			continue
		default:
			// 只有 failed / canceled 才是真正的「上游死了」。
			return false, true, fmt.Sprintf("upstream %s ended as %s", d.IdemKey, up.State())
		}
	}
	job.markReady()
	return true, false, ""
}

func (s *Scheduler) terminalState(idemKey string) (JobState, bool) {
	st, ok := s.terminalMem[idemKey]
	return st, ok
}

// ---------------------------------------------------------------------------
// 停机
// ---------------------------------------------------------------------------

// BeginDrain 停止接收新调度决策。已经 dispatch 的作业继续跑完。
func (s *Scheduler) BeginDrain() {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
}

func (s *Scheduler) Draining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// PendingCount 返回未终结作业数。
func (s *Scheduler) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lenWaitingLocked() + s.ready.Len()
}

// CancelPending 强制取消所有尚未开始执行的作业（停机超时后的清场）。
func (s *Scheduler) CancelPending(why string) []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Job
	for _, j := range s.ready.items {
		j.markCanceled(why)
		s.finishLocked(j, StateCanceled, errors.New(why))
		out = append(out, j)
	}
	s.ready.items = nil
	heap.Init(&s.ready)

	for _, j := range s.waiting {
		j.markCanceled(why)
		s.finishLocked(j, StateCanceled, errors.New(why))
		out = append(out, j)
	}
	s.waiting = nil
	return out
}

// MarkTenantRunning 把某租户所有在途作业标为取消（上游组失败时调用）。
func (s *Scheduler) MarkGroupCanceled(group, why string) []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Job
	for _, j := range s.index {
		if j.Group == group {
			if j.State() == StateRunning || j.State() == StatePending {
				j.markCanceled(why)
				out = append(out, j)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 观测
// ---------------------------------------------------------------------------

// Shares 返回所有租户的 DRF 份额视图。
func (s *Scheduler) Shares(pool *Pool) []tenantShare {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := pool.Total()
	out := make([]tenantShare, 0, len(s.byTenant))
	for name, t := range s.byTenant {
		out = append(out, tenantShare{
			Tenant:    name,
			Usage:     t.reserved,
			Share:     dominantShare(t.reserved, total),
			Pending:   t.pending,
			Running:   t.running,
			Known:     s.knownTenants[name],
			ColdStart: !s.knownTenants[name],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Share != out[j].Share {
			return out[i].Share < out[j].Share
		}
		return out[i].Tenant < out[j].Tenant
	})
	return out
}

type SchedulerStats struct {
	Decisions     int64 `json:"decisions"`
	RejectedSize  int64 `json:"rejected_insufficient"`
	RejectedBreak int64 `json:"rejected_breaker"`
	KeyedMuWaits  int64 `json:"keyed_mutex_waits"`
	Ready         int   `json:"ready"`
	Waiting       int   `json:"waiting"`
	Active        int   `json:"active"`
}

func (s *Scheduler) Stats() SchedulerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SchedulerStats{
		Decisions:     s.decisions.Load(),
		RejectedSize:  s.rejectedCont.Load(),
		RejectedBreak: s.rejectedBreak.Load(),
		KeyedMuWaits:  s.km.Waits(),
		Ready:         s.ready.Len(),
		Waiting:       len(s.waiting),
		Active:        len(s.index),
	}
}

// ResetForRecovery 在 WAL 重放前清空调度器，让重放重建全部状态。
func (s *Scheduler) ResetForRecovery() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = readyHeap{}
	heap.Init(&s.ready)
	s.waiting = nil
	s.index = make(map[string]*Job)
	s.byTenant = make(map[string]*tenantState)
	s.terminalMem = make(map[string]JobState)
	s.warmShares = make(map[string]float64)
	s.knownTenants = make(map[string]bool)
	s.seq = 0
	s.draining = false
	s.decisions.Store(0)
	s.rejectedCont.Store(0)
	s.rejectedBreak.Store(0)
}
