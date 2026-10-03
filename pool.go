// pool.go — 共享算力池：总量/已用量的原子记账 + 按租户聚合，支持扣减与释放。
//
// 设计要点：
//   - 全局计数器（Total/Used 三维）用 atomic.Int64 记账，扣减路径上不与租户账本
//     争锁，因此"高锁竞争下 <100µs"的指标主要由租户表的分片锁决定。
//   - 租户账本（按租户聚合的已用量）用分片互斥：租户名哈希到固定分片，
//     多租户并发扣减几乎不互相阻塞；同租户串行以保证读改写原子性。
//   - 两阶段扣减（Reserve -> Commit/Release）：Reserve 只做内存预占并返回一张
//     租约（Lease），真正生效与回滚由 runner 在作业状态确定后驱动，
//     保证"失败回滚"与"取消排空"路径不会漏账或双账。
package main

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"sync/atomic"
)

// Resources 是一次作业的资源请求（CPU 核数、内存字节、GPU 张数）。
type Resources struct {
	CPU int64 `json:"cpu"`
	Mem int64 `json:"mem"`
	GPU int64 `json:"gpu"`
}

// Validate 校验资源请求：三项均不可为负，且不能全零（否则是无意义作业）。
func (r Resources) Validate() error {
	if r.CPU < 0 || r.Mem < 0 || r.GPU < 0 {
		return fmt.Errorf("资源不可为负: %+v", r)
	}
	if r.CPU == 0 && r.Mem == 0 && r.GPU == 0 {
		return fmt.Errorf("资源请求不可全零: %+v", r)
	}
	return nil
}

// IsZero 判断请求是否为空。
func (r Resources) IsZero() bool { return r.CPU == 0 && r.Mem == 0 && r.GPU == 0 }

// Add 逐维相加。
func (r Resources) Add(o Resources) Resources {
	return Resources{CPU: r.CPU + o.CPU, Mem: r.Mem + o.Mem, GPU: r.GPU + o.GPU}
}

// Sub 逐维相减。
func (r Resources) Sub(o Resources) Resources {
	return Resources{CPU: r.CPU - o.CPU, Mem: r.Mem - o.Mem, GPU: r.GPU - o.GPU}
}

// Fits 判断在给定已用量下是否装得下（不小于 0 即装得下）。
func (r Resources) Fits(total, used Resources) bool {
	return used.CPU+r.CPU <= total.CPU &&
		used.Mem+r.Mem <= total.Mem &&
		used.GPU+r.GPU <= total.GPU
}

// String 便于终端打印。
func (r Resources) String() string {
	return fmt.Sprintf("cpu=%d mem=%s gpu=%d", r.CPU, humanBytes(r.Mem), r.GPU)
}

func humanBytes(b int64) string {
	const u = 1024
	if b >= u*u*u {
		return fmt.Sprintf("%.1fGiB", float64(b)/(u*u*u))
	}
	if b >= u*u {
		return fmt.Sprintf("%.1fMiB", float64(b)/(u*u))
	}
	if b >= u {
		return fmt.Sprintf("%.1fKiB", float64(b)/u)
	}
	return fmt.Sprintf("%dB", b)
}

// dimension names for reporting
const (
	dimCPU = iota
	dimMem
	dimGPU
	dimCount
)

var dimNames = [dimCount]string{"cpu", "mem", "gpu"}

// Pool 是共享算力池。
type Pool struct {
	total [dimCount]atomic.Int64
	used  [dimCount]atomic.Int64

	// usedPacked 是三维已用量的打包真源（单一 CAS 保障多维预留的原子性）。
	// used[] 只是它的只读镜像，供展示与快速读取。
	usedPacked atomic.Uint64

	// 分片租户账本：shard.mu 保护 shard.m[tenant]。
	shards []tenantShard

	// peakUsed 记录历史峰值，供终端展示与容量规划。
	peakUsed [dimCount]atomic.Int64

	// reserve/complete 计数器，供自检与 demo 输出。
	nReserve   atomic.Int64
	nCommit    atomic.Int64
	nRelease   atomic.Int64
	nRejected  atomic.Int64
	nOvershoot atomic.Int64
}

type tenantShard struct {
	mu sync.Mutex
	m  map[string]*Resources
}

const tenantShardCount = 64

// NewPool 创建算力池。total 即集群规格（题面：64 核 / 256GB / 8 GPU）。
func NewPool(total Resources) *Pool {
	p := &Pool{shards: make([]tenantShard, tenantShardCount)}
	for i := range p.shards {
		p.shards[i].m = make(map[string]*Resources)
	}
	p.total[dimCPU].Store(total.CPU)
	p.total[dimMem].Store(total.Mem)
	p.total[dimGPU].Store(total.GPU)
	return p
}

// Total 返回池总量。
func (p *Pool) Total() Resources {
	return Resources{
		CPU: p.total[dimCPU].Load(),
		Mem: p.total[dimMem].Load(),
		GPU: p.total[dimGPU].Load(),
	}
}

// Used 返回全池已用量。
func (p *Pool) Used() Resources {
	return Resources{
		CPU: p.used[dimCPU].Load(),
		Mem: p.used[dimMem].Load(),
		GPU: p.used[dimGPU].Load(),
	}
}

// Free 返回剩余可用量。
func (p *Pool) Free() Resources {
	t, u := p.Total(), p.Used()
	return t.Sub(u)
}

func (p *Pool) shardFor(tenant string) *tenantShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(tenant))
	return &p.shards[int(h.Sum32()%tenantShardCount)]
}

// TenantUsed 返回某租户当前占用（按聚合口径）。
func (p *Pool) TenantUsed(tenant string) Resources {
	s := p.shardFor(tenant)
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[tenant]; ok {
		return *v
	}
	return Resources{}
}

// TryReserve 尝试为作业预占资源；成功返回租约，失败返回原因（不修改任何账本）。
//
// 两阶段语义的关键：这里只把资源"挂"到租户账本上（预占），对全局 used 的可见性
// 立即生效（这样其它租户的 DRF 决策能看到真实占用），但允许同一作业失败后
// Release 回滚，因此绝不允许 used 出现负数或越过 total。
func (p *Pool) TryReserve(tenant string, req Resources) (Lease, error) {
	if err := req.Validate(); err != nil {
		p.nRejected.Add(1)
		return Lease{}, err
	}
	if tenant == "" {
		p.nRejected.Add(1)
		return Lease{}, fmt.Errorf("租户为空")
	}
	// 快路径：先用全局原子做无锁预判，绝大多数不冲突的扣减在此返回。
	// 用 Add 之前先 Load 是乐观读；真正的强一致由下面的 CAS 循环保证。
	if !p.tryReserveGlobal(req) {
		p.nRejected.Add(1)
		t, u := p.Total(), p.Used()
		return Lease{}, &InsufficientError{Req: req, Total: t, Used: u}
	}

	// 记账到租户账本。分片锁只保护本分片，不影响其它租户。
	s := p.shardFor(tenant)
	s.mu.Lock()
	cur, ok := s.m[tenant]
	if !ok {
		cur = &Resources{}
		s.m[tenant] = cur
	}
	*cur = cur.Add(req)
	s.mu.Unlock()

	p.nReserve.Add(1)
	return Lease{Pool: p, Tenant: tenant, Req: req, st: &leaseState{}}, nil
}

// tryReserveGlobal 用 CAS 循环在三维上原子地预留资源。
//
// 多维需要"要么全成、要么全不成"，因此这里用一个全局 CAS 打包计数：
// 把三维已用量按固定位宽打包进一个 uint64，再用单一 CAS 完成多维原子预留。
// 打包布局（位宽 21/38/5，合计 64 位）能表示 2M 核 / 256TB 内存 / 31 张 GPU，
// 远超题面 64 核 / 256GB / 8 GPU 的规模。
func (p *Pool) tryReserveGlobal(req Resources) bool {
	const (
		cpuBits = 21
		memBits = 38
		gpuBits = 5
	)
	t := p.Total()
	for {
		cur := p.usedPacked.Load()
		cpu := unpackCPU(cur)
		mem := unpackMem(cur)
		gpu := unpackGPU(cur)

		nc, nm, ng := cpu+req.CPU, mem+req.Mem, gpu+req.GPU
		// 任一维越界即整体拒绝，保证"要么全成、要么全不成"。
		if nc > t.CPU || nm > t.Mem || ng > t.GPU {
			return false
		}
		next := packUsed(nc, nm, ng)
		if p.usedPacked.CompareAndSwap(cur, next) {
			p.syncUsedFromPacked()
			p.trackPeak()
			return true
		}
	}
}

// 打包位宽：cpu 21 位 / mem 38 位 / gpu 5 位，合计 64 位。
// 上界分别为 2M 核、256TiB、31 GPU，远超 64 核 / 256GB / 8 GPU 的题面规模。
const (
	packMemBits  = 38
	packGPUBits  = 5
	packCPUShift = packMemBits + packGPUBits
	packGPUMask  = (1 << packGPUBits) - 1
	packMemMask  = (1 << packMemBits) - 1
	packCPUMask  = (1 << (64 - packCPUShift)) - 1
)

func packUsed(cpu, mem, gpu int64) uint64 {
	return uint64(cpu&packCPUMask)<<packCPUShift |
		uint64(mem&packMemMask)<<packGPUBits |
		uint64(gpu&packGPUMask)
}

func unpackCPU(v uint64) int64 { return int64((v >> packCPUShift) & packCPUMask) }
func unpackMem(v uint64) int64 { return int64((v >> packGPUBits) & packMemMask) }
func unpackGPU(v uint64) int64 { return int64(v & packGPUMask) }

// syncUsedFromPacked 把打包计数镜像到分维原子量，仅供展示/读路径使用。
func (p *Pool) syncUsedFromPacked() {
	cur := p.usedPacked.Load()
	p.used[dimCPU].Store(unpackCPU(cur))
	p.used[dimMem].Store(unpackMem(cur))
	p.used[dimGPU].Store(unpackGPU(cur))
}

func (p *Pool) trackPeak() {
	cur := p.usedPacked.Load()
	bumpPeak(&p.peakUsed[dimCPU], unpackCPU(cur))
	bumpPeak(&p.peakUsed[dimMem], unpackMem(cur))
	bumpPeak(&p.peakUsed[dimGPU], unpackGPU(cur))
}

func bumpPeak(peak *atomic.Int64, v int64) {
	for {
		cur := peak.Load()
		if v <= cur || peak.CompareAndSwap(cur, v) {
			return
		}
	}
}

// releaseGlobal 从打包计数中归还资源。
func (p *Pool) releaseGlobal(req Resources) {
	for {
		cur := p.usedPacked.Load()
		cpu, mem, gpu := unpackCPU(cur), unpackMem(cur), unpackGPU(cur)
		// 归还时钳到 0：任何路径都不允许 used 变负（宁可记账偏差也不许负值扩散）。
		nc, nm, ng := cpu-req.CPU, mem-req.Mem, gpu-req.GPU
		if nc < 0 {
			nc = 0
			p.nOvershoot.Add(1)
		}
		if nm < 0 {
			nm = 0
			p.nOvershoot.Add(1)
		}
		if ng < 0 {
			ng = 0
			p.nOvershoot.Add(1)
		}
		next := packUsed(nc, nm, ng)
		if p.usedPacked.CompareAndSwap(cur, next) {
			p.syncUsedFromPacked()
			return
		}
	}
}

// leaseState 是租约的可变状态。放在指针后面，使 Lease 本身可以被自由按值传递
// （atomic.Bool 含 noCopy，按值复制会破坏 CAS 语义并触发 vet 告警）。
type leaseState struct {
	committed atomic.Bool
	released  atomic.Bool
}

// Lease 是一次资源预留的凭据；同一租约只能 Commit 或 Release 一次（由 CAS 保证）。
type Lease struct {
	Pool   *Pool
	Tenant string
	Req    Resources

	st *leaseState
}

// Committed 表示该租约是否已确认生效。
func (l Lease) Committed() bool { return l.st != nil && l.st.committed.Load() }

// Commit 把预占转为正式占用（作业真正开始执行时调用）。
func (l Lease) Commit() bool {
	if l.st == nil {
		return false
	}
	if l.st.committed.CompareAndSwap(false, true) {
		l.Pool.nCommit.Add(1)
		return true
	}
	return false
}

// Release 归还预占资源；幂等，重复调用无副作用（防止 errgroup 取消与重试双回滚）。
func (l Lease) Release() bool {
	if l.st == nil {
		return false
	}
	if !l.st.released.CompareAndSwap(false, true) {
		return false
	}
	l.Pool.releaseGlobal(l.Req)
	s := l.Pool.shardFor(l.Tenant)
	s.mu.Lock()
	if cur, ok := s.m[l.Tenant]; ok {
		*cur = cur.Sub(l.Req)
		if cur.IsZero() {
			delete(s.m, l.Tenant)
		}
	}
	s.mu.Unlock()
	l.Pool.nRelease.Add(1)
	return true
}

// TenantSnapshot 是租户资源占用的只读快照。
type TenantSnapshot struct {
	Tenant string    `json:"tenant"`
	Used   Resources `json:"used"`
}

// Tenants 返回所有当前有占用的租户快照，按租户名排序。
func (p *Pool) Tenants() []TenantSnapshot {
	out := []TenantSnapshot{}
	seen := map[string]*Resources{}
	for i := range p.shards {
		p.shards[i].mu.Lock()
		for k, v := range p.shards[i].m {
			cp := *v
			seen[k] = &cp
		}
		p.shards[i].mu.Unlock()
	}
	for k, v := range seen {
		out = append(out, TenantSnapshot{Tenant: k, Used: *v})
	}
	sortTenantSnaps(out)
	return out
}

func sortTenantSnaps(s []TenantSnapshot) {
	sort.Slice(s, func(i, j int) bool { return s[i].Tenant < s[j].Tenant })
}

// ResetTenant 把某租户占用清零（仅用于 WAL 重放前的账本重建，不在运行期调用）。
func (p *Pool) ResetTenant(tenant string) {
	s := p.shardFor(tenant)
	s.mu.Lock()
	delete(s.m, tenant)
	s.mu.Unlock()
}

// Stats 是算力池计数快照。
type Stats struct {
	Reserve  int64 `json:"reserve"`
	Commit   int64 `json:"commit"`
	Release  int64 `json:"release"`
	Rejected int64 `json:"rejected"`
	PeakCPU  int64 `json:"peak_cpu"`
	PeakMem  int64 `json:"peak_mem"`
	PeakGPU  int64 `json:"peak_gpu"`
}

// Stats 返回池的统计计数。
func (p *Pool) Stats() Stats {
	return Stats{
		Reserve:  p.nReserve.Load(),
		Commit:   p.nCommit.Load(),
		Release:  p.nRelease.Load(),
		Rejected: p.nRejected.Load(),
		PeakCPU:  p.peakUsed[dimCPU].Load(),
		PeakMem:  p.peakUsed[dimMem].Load(),
		PeakGPU:  p.peakUsed[dimGPU].Load(),
	}
}

// InsufficientError 表示资源不足（不是系统错误，而是正常的背压信号）。
type InsufficientError struct {
	Req   Resources
	Total Resources
	Used  Resources
}

func (e *InsufficientError) Error() string {
	free := e.Total.Sub(e.Used)
	return fmt.Sprintf("资源不足: 需要 %s，可用 %s（总量 %s / 已用 %s）",
		e.Req, free, e.Total, e.Used)
}
