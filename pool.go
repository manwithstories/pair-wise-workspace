package main

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// PoolSnapshot 是资源池某一时刻的深拷贝视图，给 status 子命令和 WAL 用。
type PoolSnapshot struct {
	Total     Resources            `json:"total"`
	Allocated Resources            `json:"allocated"`
	ByTenant  map[string]Resources `json:"by_tenant"`
}

// Pool 是共享算力池。它要做三件事：
//
//  1. 用原子计数器维护「总量 / 已分配量」，让 status 查询完全无锁；
//  2. 维护一张按租户聚合的账本，供 DRF 计算各租户份额；
//  3. 充当 Allotter 接口，让 runner 能扣减与释放资源。
//
// 计数器走原子、租户账本走互斥锁，是刻意的取舍：status 是高频只读路径，
// 而扣减本身已经是调度器的全局串行点（调度决策串行提交执行），
// 所以临界区非常短，高锁竞争下的 p99 仍是亚微秒级。
type Pool struct {
	total Resources

	// 原子镜像，保证读路径零锁。
	allocCPU   atomic.Int64
	allocMemMB atomic.Int64
	allocGPU   atomic.Int64

	mu sync.Mutex
	// byTenant[i] 与 names[i] 并行存储，避免 map 迭代随机性影响可复现的调度轨迹。
	byTenant []Resources
	names    []string
	index    map[string]int

	// held 记录「当前正被某个作业占用」的键（租户 + 幂等键）。
	//
	// 它有两个作用，且必须正确处理重试：
	//  1. 归还幂等：重复 release 同一个键时直接忽略，防止账本被减成负数；
	//  2. 拒绝重复扣减：同一个键同时持有两份资源会让总账超卖。
	//
	// 关键设计：release 时只是把键「标记为已归还过」，但 Acquire 时要先把
	// 这个标记清掉再重新登记。如果把「已归还」当成永久墓碑，那么作业
	// 第一次执行完归还资源后，任何重试都会被判成 duplicate acquire 而永久失败
	// ——表现就是 RETRY 日志打出来之后，作业再也没被调度过，直接静默卡死。
	held          map[string]Resources
	releasedGuard map[string]struct{}

	// ---- 统计 ----
	acquireCount atomic.Int64
	releaseCount atomic.Int64
	contended    atomic.Int64 // 因资源不足而返回 false 的次数
	rejectCount  atomic.Int64 // 因资源量超过池容量而返回 false 的次数
	maxWaitNanos atomic.Int64 // acquire 临界区内观测到的最长持锁时间
}

func NewPool(total Resources) (*Pool, error) {
	if total.CPU <= 0 || total.MemMB <= 0 || total.GPU <= 0 {
		return nil, fmt.Errorf("pool capacity must be positive on all three dimensions, got %s", total)
	}
	p := &Pool{
		total:         total,
		index:         make(map[string]int),
		held:          make(map[string]Resources),
		releasedGuard: make(map[string]struct{}),
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 读路径（无锁）
// ---------------------------------------------------------------------------

func (p *Pool) Total() Resources { return p.total }

func (p *Pool) Allocated() Resources {
	return Resources{
		CPU:   p.allocCPU.Load(),
		MemMB: p.allocMemMB.Load(),
		GPU:   p.allocGPU.Load(),
	}
}

func (p *Pool) Available() Resources {
	a := p.Allocated()
	return Resources{CPU: p.total.CPU - a.CPU, MemMB: p.total.MemMB - a.MemMB, GPU: p.total.GPU - a.GPU}
}

func (p *Pool) Utilisation() Resources {
	t := p.total
	return Resources{
		CPU:   pct(p.allocCPU.Load(), t.CPU),
		MemMB: pct(p.allocMemMB.Load(), t.MemMB),
		GPU:   pct(p.allocGPU.Load(), t.GPU),
	}
}

func pct(used, total int64) int64 {
	if total <= 0 {
		return 0
	}
	return used * 100 / total
}

func (p *Pool) Stats() PoolStats {
	return PoolStats{
		AcquireCalls: p.acquireCount.Load(),
		ReleaseCalls: p.releaseCount.Load(),
		Contended:    p.contended.Load(),
		Rejected:     p.rejectCount.Load(),
		MaxLockNanos: p.maxWaitNanos.Load(),
	}
}

type PoolStats struct {
	AcquireCalls int64
	ReleaseCalls int64
	Contended    int64
	Rejected     int64
	MaxLockNanos int64
}

// ---------------------------------------------------------------------------
// 写路径（短临界区）
// ---------------------------------------------------------------------------

// Acquire 为作业扣减资源。要求三维同时满足才扣减，避免「卡扣了但内存扣不下」
// 这种撕裂式分配让 GPU 空转。返回 false 有三种含义：资源量本身超过池容量、
// 池已被占满、或同键重复扣减（后者属于调用方 bug）。
func (p *Pool) Acquire(tenant string, key string, want Resources) error {
	p.acquireCount.Add(1)
	start := time.Now()

	p.mu.Lock()
	// 先注册统计（后执行），再注册解锁（先执行），保证临界区一定被释放。
	defer func() {
		if d := int64(time.Since(start)); d > p.maxWaitNanos.Load() {
			p.maxWaitNanos.Store(d)
		}
	}()
	defer p.mu.Unlock()

	// 单个作业的请求就超过池容量，永远排不上队，直接拒。
	if !want.Fits(p.total) {
		p.rejectCount.Add(1)
		return fmt.Errorf("request %s exceeds pool capacity %s", want, p.total)
	}
	gk := guardKey(tenant, key)
	if prev, dup := p.held[gk]; dup {
		// 同一作业已经在占用资源了。再扣一份就是双倍占用——
		// 这正是「手抖重复提交导致 GPU 被双份占满」在资源池层面的最后防线。
		p.rejectCount.Add(1)
		return fmt.Errorf("duplicate acquire for %s (already holding %s)", key, prev)
	}

	avail := Resources{
		CPU:   p.total.CPU - p.allocCPU.Load(),
		MemMB: p.total.MemMB - p.allocMemMB.Load(),
		GPU:   p.total.GPU - p.allocGPU.Load(),
	}
	if !want.Fits(avail) {
		p.contended.Add(1)
		return ErrInsufficient
	}

	i := p.tenantIndexLocked(tenant)
	p.byTenant[i] = addRes(p.byTenant[i], want)
	p.held[gk] = want
	p.allocCPU.Add(want.CPU)
	p.allocMemMB.Add(want.MemMB)
	p.allocGPU.Add(want.GPU)
	return nil
}

// Release 归还资源。重复归还是幂等的：第二次直接返回 nil，不改账本。
// 这是「部分结果不得泄漏」的最后一道保险——即使 errgroup 取消路径被重复
// 触发，GPU 也不会被重复加回成超卖状态。
func (p *Pool) Release(tenant string, key string, held Resources) {
	p.releaseCount.Add(1)

	p.mu.Lock()
	defer p.mu.Unlock()

	gk := guardKey(tenant, key)
	// 只有当前确实持有资源时才允许归还；没持有就是重复 release，直接忽略。
	cur, ok := p.held[gk]
	if !ok {
		return // 幂等：已经还过了（取消路径可能重复触发）
	}
	if cur != held && cur.CPU < held.CPU {
		// 归还量大于持有量，说明调用方记账有问题。
		held = cur
	}
	delete(p.held, gk)

	i := p.tenantIndexLocked(tenant)
	after := subRes(p.byTenant[i], held)
	if after.CPU < 0 || after.MemMB < 0 || after.GPU < 0 {
		// 账本被减成负数说明上层出错，宁可立刻暴露也不要继续跑。
		panic(fmt.Sprintf("pool underflow releasing %s from tenant %s: %s -> %s", held, tenant, p.byTenant[i], after))
	}
	p.byTenant[i] = after
	p.allocCPU.Add(-held.CPU)
	p.allocMemMB.Add(-held.MemMB)
	p.allocGPU.Add(-held.GPU)
}

func guardKey(tenant, key string) string { return tenant + "\x00" + key }

func (p *Pool) tenantIndexLocked(tenant string) int {
	if i, ok := p.index[tenant]; ok {
		return i
	}
	i := len(p.names)
	p.names = append(p.names, tenant)
	p.byTenant = append(p.byTenant, Resources{})
	p.index[tenant] = i
	return i
}

// TenantUsage 返回某租户当前占用的三维资源。
func (p *Pool) TenantUsage(tenant string) Resources {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i, ok := p.index[tenant]; ok {
		return p.byTenant[i]
	}
	return Resources{}
}

// Tenants 返回所有出现过租户的名字，按占用降序、名字升序排列。
func (p *Pool) Tenants() []string {
	p.mu.Lock()
	out := make([]string, 0, len(p.names))
	usage := make(map[string]Resources, len(p.names))
	for i, n := range p.names {
		out = append(out, n)
		usage[n] = p.byTenant[i]
	}
	p.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		ui, uj := usage[out[i]], usage[out[j]]
		if ui.dominant() != uj.dominant() {
			return ui.dominant() > uj.dominant()
		}
		return out[i] < out[j]
	})
	return out
}

// Snapshot 拍一张完整账本快照。
func (p *Pool) Snapshot() PoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := make(map[string]Resources, len(p.names))
	for i, n := range p.names {
		m[n] = p.byTenant[i]
	}
	return PoolSnapshot{
		Total:     p.total,
		Allocated: Resources{p.allocCPU.Load(), p.allocMemMB.Load(), p.allocGPU.Load()},
		ByTenant:  m,
	}
}

func (s PoolSnapshot) String() string {
	return fmt.Sprintf("allocated %s / total %s (%d%%/%d%%/%d%%)",
		s.Allocated, s.Total,
		pct(s.Allocated.CPU, s.Total.CPU), pct(s.Allocated.MemMB, s.Total.MemMB), pct(s.Allocated.GPU, s.Total.GPU))
}

// ErrInsufficient 表示三维里至少有一维放不下。注意它和「请求超过池容量」
// 是两回事：后者是永久性错误（重试也没用），前者只是「现在不行，等一等」。
var ErrInsufficient = errors.New("insufficient pool resources")
