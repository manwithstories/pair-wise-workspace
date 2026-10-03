// scheduler_test.go — 针对题面逐条要求的单元/集成测试。
//
// 覆盖：DRF 边界（零请求/超额/并列/冷启动）、幂等去重与执行期互斥、
// 依赖与级联取消、errgroup 取消传播、指数退避与熔断半开放、
// WAL 重放恢复、优雅停机、资源池原子性。
package main

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- 测试脚手架 ----------------------------------------------------------

func newTestApp(t *testing.T, total Resources) (*app, func()) {
	t.Helper()
	dir := t.TempDir()
	opt := appOptions{
		walPath: filepath.Join(dir, "test.wal"),
		total:   total,
		quiet:   true,
	}
	a, err := newApp(opt)
	if err != nil {
		t.Fatalf("newApp 失败: %v", err)
	}
	return a, func() { _ = a.wal.Close() }
}

func smallPool() Resources { return Resources{CPU: 16, Mem: 4 << 30, GPU: 4} }

// waitState 轮询等待作业到达期望状态。
func waitState(t *testing.T, s *Scheduler, id string, want JobState, timeout time.Duration) *Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if j := s.Lookup(id); j != nil {
			if j.StateOf() == want {
				return j
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	j := s.Lookup(id)
	got := "<nil>"
	if j != nil {
		got = string(j.StateOf())
	}
	t.Fatalf("作业 %s 期望状态 %s，实际 %s（超时 %s）", id, want, got, timeout)
	return nil
}

// waitRunning 等待作业进入 running。
func waitRunning(t *testing.T, s *Scheduler, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if j := s.Lookup(id); j != nil && j.StateOf() == StateRunning {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if j := s.Lookup(id); j != nil {
		t.Fatalf("作业 %s 期望进入 running，实际 %s", id, j.StateOf())
	}
	t.Fatalf("作业 %s 不存在", id)
}

// ---- 1. 资源池：零请求、超额、原子性 --------------------------------------

func TestPoolRejectsZeroRequest(t *testing.T) {
	p := NewPool(smallPool())
	if err := (Resources{}).Validate(); err == nil {
		t.Fatal("全零资源请求应当被拒绝")
	}
	if _, err := p.TryReserve("t", Resources{}); err == nil {
		t.Fatal("全零资源请求不应当被预留")
	}
}

func TestPoolRejectsNegativeRequest(t *testing.T) {
	p := NewPool(smallPool())
	if err := (Resources{CPU: -1}).Validate(); err == nil {
		t.Fatal("负数资源请求应当被拒绝")
	}
	if _, err := p.TryReserve("t", Resources{CPU: -1, Mem: 1}); err == nil {
		t.Fatal("负数资源请求不应当被预留")
	}
}

func TestPoolRejectsOverCapacity(t *testing.T) {
	p := NewPool(smallPool())
	if _, err := p.TryReserve("t", Resources{CPU: 100, Mem: 1}); err == nil {
		t.Fatal("超出总量的请求应当被拒绝")
	}
	// 三维里任一维超了都要整体拒绝。
	if _, err := p.TryReserve("t", Resources{CPU: 1, Mem: 1 << 40}); err == nil {
		t.Fatal("内存超限的请求应当被拒绝")
	}
	if _, err := p.TryReserve("t", Resources{CPU: 1, Mem: 1, GPU: 99}); err == nil {
		t.Fatal("GPU 超限的请求应当被拒绝")
	}
}

func TestPoolNoLeakOnPartialReject(t *testing.T) {
	p := NewPool(smallPool())
	_, _ = p.TryReserve("t", Resources{CPU: 1, Mem: 1, GPU: 99}) // GPU 超限
	if p.Used() != (Resources{}) {
		t.Fatalf("被拒绝的预留不应留下占用账本，实际 %s", p.Used())
	}
}

func TestPoolReserveReleaseIdempotent(t *testing.T) {
	p := NewPool(smallPool())
	l, err := p.TryReserve("t", Resources{CPU: 2, Mem: 1 << 20})
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if !l.Release() {
		t.Fatal("首次 Release 应成功")
	}
	if l.Release() {
		t.Fatal("重复 Release 应返回 false（幂等）")
	}
	if p.Used() != (Resources{}) {
		t.Fatalf("释放后应无占用，实际 %s", p.Used())
	}
	if p.TenantUsed("t") != (Resources{}) {
		t.Fatalf("租户账本应清零，实际 %s", p.TenantUsed("t"))
	}
}

func TestPoolConcurrentNeverOvershoots(t *testing.T) {
	p := NewPool(Resources{CPU: 64, Mem: 1 << 30, GPU: 8})
	var wg sync.WaitGroup
	var granted int64
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 200; k++ {
				tenant := fmt.Sprintf("t%d", i%16)
				l, err := p.TryReserve(tenant, Resources{CPU: 1, Mem: 1 << 20})
				if err == nil {
					atomic.AddInt64(&granted, 1)
					l.Release()
				}
			}
		}(i)
	}
	wg.Wait()
	u := p.Used()
	if u.CPU < 0 || u.Mem < 0 || u.GPU < 0 {
		t.Fatalf("并发后出现负占用: %s", u)
	}
	if u.CPU > 64 || u.Mem > (1<<30) || u.GPU > 8 {
		t.Fatalf("并发后超卖: %s", u)
	}
	if u != (Resources{}) {
		t.Fatalf("全部释放后应为 0，实际 %s", u)
	}
}

// ---- 2. 幂等提交 ----------------------------------------------------------

func TestIdempotentSubmitReturnsExistingHandle(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()

	spec := JobSpec{ID: "j1", Tenant: "t1", Kind: KindNoop, IdemKey: "k1",
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: noopTask}
	h1, created1, err := a.sched.Submit(spec)
	if err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	if !created1 {
		t.Fatal("首次提交应当是新建作业")
	}
	h2, created2, err := a.sched.Submit(spec)
	if err != nil {
		t.Fatalf("重复提交失败: %v", err)
	}
	if created2 {
		t.Fatal("重复提交不应新建作业")
	}
	if h1 != h2 {
		t.Fatal("重复提交应返回同一个作业句柄")
	}
	// 队列里只应有一个。
	if n := len(a.sched.Jobs()); n != 1 {
		t.Fatalf("重复提交后作业数应为 1，实际 %d", n)
	}
}

func TestIdempotencyBlocksDoubleGPUReservation(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	var concurrent atomic.Int32
	run := func(ctx context.Context, j *Job) error {
		concurrent.Add(1)
		time.Sleep(80 * time.Millisecond)
		return nil
	}
	spec := JobSpec{ID: "gpu-job", Tenant: "t1", Kind: KindNoop, IdemKey: "same-key",
		Req: Resources{CPU: 2, Mem: 1 << 30, GPU: 2}, Run: run}

	if _, created, _ := a.sched.Submit(spec); !created {
		t.Fatal("首次提交应为新建")
	}
	// 等它真正开始跑，再重复提交同键。
	time.Sleep(30 * time.Millisecond)
	if _, created, _ := a.sched.Submit(spec); created {
		t.Fatal("执行期重复提交不应新建作业")
	}
	waitState(t, a.sched, "gpu-job", StateSucceeded, 2*time.Second)
	if got := concurrent.Load(); got != 1 {
		t.Fatalf("同键作业应只执行 1 次，实际执行 %d 次（GPU 被双份占用）", got)
	}
}

func TestOversizedJobRejected(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	_, _, err := a.sched.Submit(JobSpec{ID: "huge", Tenant: "t", Kind: KindNoop,
		Req: Resources{CPU: 9999, Mem: 1}, Run: noopTask})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超大作业应当被拒绝，实际 err=%v", err)
	}
}

func TestSubmitWithUnknownDependencyRejected(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	_, _, err := a.sched.Submit(JobSpec{ID: "d", Tenant: "t", Kind: KindNoop,
		DependsOn: []string{"nonexistent"}, Req: Resources{CPU: 1, Mem: 1}, Run: noopTask})
	if err == nil {
		t.Fatal("依赖不存在的上游应当被拒绝")
	}
}

// ---- 3. 依赖与级联取消 ---------------------------------------------------

func TestDependencyBlocksUntilUpstreamSucceeds(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	var downRan atomic.Bool
	_, _, _ = a.sched.Submit(JobSpec{ID: "up", Tenant: "t", Kind: KindNoop,
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: sleepTask(120 * time.Millisecond)})
	_, _, _ = a.sched.Submit(JobSpec{ID: "down", Tenant: "t", Kind: KindNoop,
		DependsOn: []string{"up"}, Req: Resources{CPU: 1, Mem: 1 << 20},
		Run: func(ctx context.Context, j *Job) error { downRan.Store(true); return nil }})

	// 下游在上游完成前绝不能启动。
	time.Sleep(50 * time.Millisecond)
	if downRan.Load() {
		t.Fatal("下游不应在上游完成前启动")
	}
	waitState(t, a.sched, "down", StateSucceeded, 2*time.Second)
	if !downRan.Load() {
		t.Fatal("上游成功后下游应被执行")
	}
}

func TestUpstreamFailureCascadesCancelDownstream(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	var downRan atomic.Bool
	_, _, _ = a.sched.Submit(JobSpec{ID: "up-fail", Tenant: "t", Kind: KindNoop, MaxAttempts: 1,
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: failTask("上游炸了", 10*time.Millisecond)})
	_, _, _ = a.sched.Submit(JobSpec{ID: "down-stream", Tenant: "t", Kind: KindNoop,
		DependsOn: []string{"up-fail"}, Req: Resources{CPU: 1, Mem: 1 << 20},
		Run: func(ctx context.Context, j *Job) error { downRan.Store(true); return nil }})

	waitState(t, a.sched, "up-fail", StateFailed, 3*time.Second)
	waitState(t, a.sched, "down-stream", StateCanceled, 3*time.Second)
	if downRan.Load() {
		t.Fatal("上游失败后下游不应被执行（不应空占资源）")
	}
}

func TestCascadeIsTransitive(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	_, _, _ = a.sched.Submit(JobSpec{ID: "root", Tenant: "t", Kind: KindNoop, MaxAttempts: 1,
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: failTask("根节点失败", 10*time.Millisecond)})
	_, _, _ = a.sched.Submit(JobSpec{ID: "mid", Tenant: "t", Kind: KindNoop,
		DependsOn: []string{"root"}, Req: Resources{CPU: 1, Mem: 1 << 20}, Run: noopTask})
	_, _, _ = a.sched.Submit(JobSpec{ID: "leaf", Tenant: "t", Kind: KindNoop,
		DependsOn: []string{"mid"}, Req: Resources{CPU: 1, Mem: 1 << 20}, Run: noopTask})

	waitState(t, a.sched, "root", StateFailed, 3*time.Second)
	waitState(t, a.sched, "mid", StateCanceled, 3*time.Second)
	waitState(t, a.sched, "leaf", StateCanceled, 3*time.Second)
}

// ---- 4. errgroup 取消传播 ------------------------------------------------

func TestErrgroupCancelsSiblingsOnFailure(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	var siblingCanceled atomic.Bool
	_, _, _ = a.sched.Submit(JobSpec{ID: "g-ok", Tenant: "t", Kind: KindNoop, Group: "G",
		Req: Resources{CPU: 1, Mem: 1 << 20},
		Run: func(ctx context.Context, j *Job) error {
			select {
			case <-time.After(3 * time.Second):
				return nil
			case <-ctx.Done():
				siblingCanceled.Store(true)
				return ctx.Err()
			}
		}})
	_, _, _ = a.sched.Submit(JobSpec{ID: "g-bad", Tenant: "t", Kind: KindNoop, Group: "G", MaxAttempts: 1,
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: failTask("组内失败", 40*time.Millisecond)})

	waitState(t, a.sched, "g-bad", StateFailed, 3*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !siblingCanceled.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	if !siblingCanceled.Load() {
		t.Fatal("组内一个作业失败后，兄弟作业应收到 context 取消信号")
	}
	waitState(t, a.sched, "g-ok", StateCanceled, 2*time.Second)
}

func TestCanceledSiblingReleasesResources(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	_, _, _ = a.sched.Submit(JobSpec{ID: "hold", Tenant: "t", Kind: KindNoop, Group: "G2",
		Req: Resources{CPU: 4, Mem: 1 << 30, GPU: 1},
		Run: func(ctx context.Context, j *Job) error {
			select {
			case <-time.After(5 * time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
	_, _, _ = a.sched.Submit(JobSpec{ID: "boom", Tenant: "t", Kind: KindNoop, Group: "G2", MaxAttempts: 1,
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: failTask("boom", 40*time.Millisecond)})

	waitState(t, a.sched, "boom", StateFailed, 3*time.Second)
	// 等所有资源归还，验证"部分结果不得泄漏"。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a.pool.Used() == (Resources{}) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if u := a.pool.Used(); u != (Resources{}) {
		t.Fatalf("组取消后资源应全部释放，实际仍占用 %s", u)
	}
	for _, ts := range a.pool.Tenants() {
		if ts.Used != (Resources{}) {
			t.Fatalf("租户 %s 账本未清零: %s", ts.Tenant, ts.Used)
		}
	}
}

// ---- 5. 重试与熔断 -------------------------------------------------------

func TestExponentialBackoffRetry(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	var attempts atomic.Int32
	_, _, _ = a.sched.Submit(JobSpec{ID: "flaky", Tenant: "t", Kind: KindNoop,
		Req: Resources{CPU: 1, Mem: 1 << 20},
		Run: func(ctx context.Context, j *Job) error {
			attempts.Add(1)
			return errors.New("临时故障")
		}})
	waitState(t, a.sched, "flaky", StateFailed, 5*time.Second)
	if got := attempts.Load(); got != int32(DefaultMaxAttempts) {
		t.Fatalf("应重试 %d 次，实际 %d 次", DefaultMaxAttempts, got)
	}
}

func TestRetryBackoffGrows(t *testing.T) {
	base := 100 * time.Millisecond
	prev := time.Duration(0)
	for attempt := 1; attempt <= 4; attempt++ {
		d := RetryBackoff(base, attempt, 1)
		if d < base/2 {
			t.Fatalf("第 %d 次退避 %s 小于基准的一半，指数增长异常", attempt, d)
		}
		if prev != 0 && d < prev {
			t.Fatalf("退避未单调增长: 第 %d 次 %s < 上次 %s", attempt, d, prev)
		}
		prev = d
	}
}

func TestCircuitBreakerOpensAndBlocks(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	// 连续 BreakerFailThreshold 个"一次即失败"的作业 -> 熔断。
	for i := 0; i < BreakerFailThreshold; i++ {
		id := fmt.Sprintf("bad-%d", i)
		_, _, _ = a.sched.Submit(JobSpec{ID: id, Tenant: "bad", Kind: KindNoop, MaxAttempts: 1,
			Req: Resources{CPU: 1, Mem: 1 << 20}, Run: failTask("必败", 5*time.Millisecond)})
		waitState(t, a.sched, id, StateFailed, 3*time.Second)
	}

	// 熔断期间，新作业应当一直停在 pending（不被调度）。
	_, _, _ = a.sched.Submit(JobSpec{ID: "after-breaker", Tenant: "bad", Kind: KindNoop,
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: func(ctx context.Context, j *Job) error {
			t.Error("熔断期间作业不应被执行")
			return nil
		}})
	time.Sleep(300 * time.Millisecond)
	j := a.sched.Lookup("after-breaker")
	if j == nil {
		t.Fatal("after-breaker 作业丢失")
	}
	if st := j.StateOf(); st != StatePending {
		t.Fatalf("熔断期间作业应保持 pending，实际 %s", st)
	}
	// 其它租户不受影响（熔断是按租户的）。
	_, _, _ = a.sched.Submit(JobSpec{ID: "healthy", Tenant: "good", Kind: KindNoop,
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: noopTask})
	waitState(t, a.sched, "healthy", StateSucceeded, 2*time.Second)
}

func TestBreakerHalfOpenAllowsSingleProbe(t *testing.T) {
	b := &tenantBreaker{}
	now := time.Now()
	for i := 0; i < BreakerFailThreshold; i++ {
		b.onFailure(now)
	}
	if b.state != breakerOpen {
		t.Fatalf("连续失败 %d 次后应为 open，实际 %v", BreakerFailThreshold, b.state)
	}
	// 冷却期内不允许任何作业。
	if b.allow(now.Add(10 * time.Second)) {
		t.Fatal("冷却期内不应放行作业")
	}
	// 冷却结束后进入半开放，只放一个探测作业。
	after := now.Add(BreakerCooldown + time.Second)
	if !b.allow(after) {
		t.Fatal("冷却结束后应放行一个探测作业")
	}
	if b.allow(after) {
		t.Fatal("半开放期间只应放行一个探测作业")
	}
	// 探测成功 -> 闭合。
	b.onSuccess()
	if b.state != breakerClosed || b.failures != 0 {
		t.Fatalf("探测成功后应复位为 closed，实际 %v failures=%d", b.state, b.failures)
	}
}

func TestBreakerProbeFailureReopens(t *testing.T) {
	b := &tenantBreaker{}
	now := time.Now()
	for i := 0; i < BreakerFailThreshold; i++ {
		b.onFailure(now)
	}
	after := now.Add(BreakerCooldown + time.Second)
	if !b.allow(after) {
		t.Fatal("应放行探测作业")
	}
	b.onFailure(after)
	if b.state != breakerOpen {
		t.Fatalf("探测失败应重新熔断，实际 %v", b.state)
	}
}

func TestRetryableFailureDoesNotTripBreaker(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	// 失败两次后第三次成功：这是"抖动"，不该熔断租户。
	var n atomic.Int32
	_, _, _ = a.sched.Submit(JobSpec{ID: "flap", Tenant: "t", Kind: KindNoop,
		Req: Resources{CPU: 1, Mem: 1 << 20},
		Run: func(ctx context.Context, j *Job) error {
			if n.Add(1) < 3 {
				return errors.New("抖动")
			}
			return nil
		}})
	waitState(t, a.sched, "flap", StateSucceeded, 5*time.Second)
	a.sched.mu.Lock()
	st := a.sched.tenants["t"].breaker.state
	a.sched.mu.Unlock()
	if st != breakerClosed {
		t.Fatalf("重试后成功的作业不应熔断租户，实际熔断状态 %v", st)
	}
}

// ---- 6. DRF 公平性 -------------------------------------------------------

func TestDRFDoesNotStarveLightTenant(t *testing.T) {
	// 核心痛点：CV 的大训练占住大部分资源时，其它团队的轻量作业不应长期饿死。
	// 池留出余量，让 DRF 有机会按"主导份额最低优先"把资源分给轻量租户。
	a, done := newTestApp(t, Resources{CPU: 10, Mem: 8 << 30, GPU: 4})
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	block := make(chan struct{})
	// hog 先占满 8 核（主导份额 0.8）。
	for i := 0; i < 2; i++ {
		_, _, _ = a.sched.Submit(JobSpec{
			ID: fmt.Sprintf("hog-%d", i), Tenant: "hog", Kind: KindNoop,
			Req: Resources{CPU: 4, Mem: 1 << 30},
			Run: func(ctx context.Context, j *Job) error { <-block; return nil }})
	}
	waitRunning(t, a.sched, "hog-1", 2*time.Second)

	// light 只�� 2 核：hog 的主导份额已远高于它，light 必须能插进来。
	_, _, _ = a.sched.Submit(JobSpec{
		ID: "light-1", Tenant: "light", Kind: KindNoop,
		Req: Resources{CPU: 2, Mem: 1 << 30},
		Run: func(ctx context.Context, j *Job) error { <-block; return nil }})
	waitState(t, a.sched, "light-1", StateRunning, 2*time.Second)

	if a.pool.TenantUsed("hog").CPU != 8 {
		t.Fatalf("hog 应占用 8 核，实际 %s", a.pool.TenantUsed("hog"))
	}
	close(block)
}

func TestDRFPrefersLowestDominantTenant(t *testing.T) {
	// 直接验证决策内核：主导份额最低的租户被选中。
	a, done := newTestApp(t, Resources{CPU: 10, Mem: 10 << 30, GPU: 4})
	defer done()

	// hog 已用满 CPU（主导 1.0），light 未用（主导 0.0）。
	if _, err := a.pool.TryReserve("hog", Resources{CPU: 10, Mem: 1 << 30}); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	_, _, _ = a.sched.Submit(JobSpec{ID: "from-hog", Tenant: "hog", Kind: KindNoop,
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: noopTask})
	_, _, _ = a.sched.Submit(JobSpec{ID: "from-light", Tenant: "light", Kind: KindNoop,
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: noopTask})

	d := a.sched.Decide()
	if d.Tenant != "light" {
		t.Fatalf("应选择主导份额最低的租户 light，实际 %s（dominant=%.3f）", d.Tenant, d.Dominant)
	}
	if d.JobID != "from-light" {
		t.Fatalf("应选择 light 的作业，实际 %s", d.JobID)
	}
}

func TestDRFTenantDominantIsMaxOfDimensions(t *testing.T) {
	// 主导份额必须取三维占比的最大值。
	tv := &tenantView{name: "t"}
	tv.used = Resources{CPU: 2, Mem: 9, GPU: 1}
	total := Resources{CPU: 10, Mem: 10, GPU: 10}
	if got := tv.dominant(total); got != 0.9 {
		t.Fatalf("主导份额应为内存占比 0.9，实际 %.3f", got)
	}
	// 某维总量为 0 时该维不参与（不得除零）。
	tv.used = Resources{CPU: 5, Mem: 0, GPU: 0}
	if got := tv.dominant(Resources{CPU: 10, Mem: 0, GPU: 0}); got != 0.5 {
		t.Fatalf("零总量维度应被忽略，期望 0.5，实际 %.3f", got)
	}
}

func TestDRFTieBreakIsFIFO(t *testing.T) {
	// 两个租户份额完全并列时，按提交序号 FIFO，不应饿死先来者。
	rq := readyQueue{}
	mk := func(id string, score float64, seq uint64) *readyItem {
		return &readyItem{job: &Job{ID: id, Tenant: "t"}, score: score, seq: seq,
			dCPU: score, dMem: score, dGPU: score}
	}
	heapPushAll(&rq, mk("late", 0.5, 20), mk("early", 0.5, 10), mk("mid", 0.5, 15))
	if got := rq[0].job.ID; got != "early" {
		t.Fatalf("并列时应取提交序号最小者，实际 %s", got)
	}
}

func TestDRFEmptyPoolDecisionIsSafe(t *testing.T) {
	p := NewPool(smallPool())
	w, _ := OpenWAL(filepath.Join(t.TempDir(), "e.wal"))
	defer w.Close()
	s := NewScheduler(p, w, func(TraceEvent) {})
	d := s.Decide()
	if d.JobID != "" {
		t.Fatalf("空池不应产生决策，实际 %s", d.JobID)
	}
}

func TestColdStartNewTenantGetsShare(t *testing.T) {
	// 新租户用量为 0，主导份额最低，DRF 应立即给它资源（短时公平）。
	a, done := newTestApp(t, Resources{CPU: 10, Mem: 8 << 30, GPU: 4})
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	block := make(chan struct{})
	_, _, _ = a.sched.Submit(JobSpec{ID: "a1", Tenant: "A", Kind: KindNoop,
		Req: Resources{CPU: 8, Mem: 1 << 30},
		Run: func(ctx context.Context, j *Job) error { <-block; return nil }})
	waitRunning(t, a.sched, "a1", 2*time.Second)

	_, _, _ = a.sched.Submit(JobSpec{ID: "b1", Tenant: "B", Kind: KindNoop,
		Req: Resources{CPU: 2, Mem: 1 << 30},
		Run: func(ctx context.Context, j *Job) error { <-block; return nil }})
	waitState(t, a.sched, "b1", StateRunning, 2*time.Second)
	close(block)
}

func TestUnfitJobRunsAfterResourcesFree(t *testing.T) {
	a, done := newTestApp(t, Resources{CPU: 2, Mem: 1 << 30, GPU: 0})
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	_, _, _ = a.sched.Submit(JobSpec{ID: "big1", Tenant: "A", Kind: KindNoop,
		Req: Resources{CPU: 2, Mem: 1 << 30}, Run: sleepTask(80 * time.Millisecond)})
	// 池只有 2 核，第二个 2 核作业暂时装不下，应被暂存而不是丢弃。
	_, _, _ = a.sched.Submit(JobSpec{ID: "big2", Tenant: "B", Kind: KindNoop,
		Req: Resources{CPU: 2, Mem: 1 << 30}, Run: sleepTask(20 * time.Millisecond)})

	waitState(t, a.sched, "big1", StateSucceeded, 3*time.Second)
	waitState(t, a.sched, "big2", StateSucceeded, 3*time.Second)
}

// ---- 7. WAL -------------------------------------------------------------

func TestWALReplayRestoresInflightJobs(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "replay.wal")

	// 第一段：提交几个作业，其中一个保持 pending 不执行完。
	w1, err := OpenWAL(walPath)
	if err != nil {
		t.Fatalf("OpenWAL 失败: %v", err)
	}
	pool1 := NewPool(smallPool())
	s1 := NewScheduler(pool1, w1, func(TraceEvent) {})
	for i := 0; i < 3; i++ {
		_, _, _ = s1.Submit(JobSpec{ID: fmt.Sprintf("j%d", i), Tenant: "t", Kind: KindNoop,
			IdemKey: fmt.Sprintf("j%d", i), Req: Resources{CPU: 1, Mem: 1 << 20}, Run: noopTask})
	}
	if err := w1.Flush(); err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// 第二段：新实例重放。
	recs, events, err := Replay(walPath)
	if err != nil {
		t.Fatalf("Replay 失败: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("应重放出 3 个作业，实际 %d", len(recs))
	}
	if len(events) == 0 {
		t.Fatal("应重放出事件")
	}
	w2, err := OpenWAL(walPath)
	if err != nil {
		t.Fatalf("二次 OpenWAL 失败: %v", err)
	}
	defer w2.Close()
	pool2 := NewPool(smallPool())
	s2 := NewScheduler(pool2, w2, func(TraceEvent) {})
	r2 := NewRunner(s2)
	s2.AttachRunner(r2)
	n := s2.ReplayRestore(recs, hydrate)
	if n != 3 {
		t.Fatalf("应恢复 3 个在途作业，实际 %d", n)
	}
	if len(s2.Jobs()) != 3 {
		t.Fatalf("重放后应有 3 个作业，实际 %d", len(s2.Jobs()))
	}
}

func TestWALReplayIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "idem.wal")
	w, _ := OpenWAL(walPath)
	for i := 0; i < 5; i++ {
		_ = w.Append(WLEvent{Event: EvtSubmit, Job: &WALJobRecord{
			ID: "dup", Tenant: "t", Kind: KindNoop, State: StatePending}})
	}
	_ = w.Close()
	recs, _, err := Replay(walPath)
	if err != nil {
		t.Fatalf("Replay 失败: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("同 ID 重复 submit 应只重放一条，实际 %d 条", len(recs))
	}
}

func TestWALTruncatesTornTail(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "torn.wal")
	w, _ := OpenWAL(walPath)
	_ = w.Append(WLEvent{Event: EvtSubmit, Job: &WALJobRecord{
		ID: "a", Tenant: "t", Kind: KindNoop, State: StatePending}})
	_ = w.Flush()
	_ = w.Close()

	// 手工追加半条记录，模拟崩溃时撕裂。
	f, err := os.OpenFile(walPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	_, _ = f.WriteString(`{"seq":99,"event":"submit","job":{"id":"brok`)
	_ = f.Close()

	// 重新打开时应截断撕裂尾部，且仍能正常追加与重放。
	w2, err := OpenWAL(walPath)
	if err != nil {
		t.Fatalf("撕裂后重新打开失败: %v", err)
	}
	_ = w2.Append(WLEvent{Event: EvtSubmit, Job: &WALJobRecord{
		ID: "b", Tenant: "t", Kind: KindNoop, State: StatePending}})
	_ = w2.Close()

	recs, _, err := Replay(walPath)
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if len(recs) != 2 || recs[0].ID != "a" || recs[1].ID != "b" {
		t.Fatalf("撕裂修复后应重放出 a、b 两条，实际 %+v", recs)
	}
}

func TestWALSeqMonotonicAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "seq.wal")
	w1, _ := OpenWAL(walPath)
	for i := 0; i < 3; i++ {
		_ = w1.Append(WLEvent{Event: EvtSubmit, Job: &WALJobRecord{ID: fmt.Sprintf("a%d", i)}})
	}
	_ = w1.Close()

	w2, _ := OpenWAL(walPath)
	defer w2.Close()
	if w2.seq < 3 {
		t.Fatalf("重开后 seq 水位应 >= 3，实际 %d", w2.seq)
	}
	_ = w2.Append(WLEvent{Event: EvtSubmit, Job: &WALJobRecord{ID: "later"}})
	if w2.seq != 4 {
		t.Fatalf("新追加 seq 应为 4，实际 %d", w2.seq)
	}
}

// ---- 8. 优雅停机 --------------------------------------------------------

func TestGracefulStopRejectsNewSubmissions(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	a.sched.BeginDrain()
	if _, _, err := a.sched.Submit(JobSpec{ID: "late", Tenant: "t", Kind: KindNoop,
		Req: Resources{CPU: 1, Mem: 1}, Run: noopTask}); !errors.Is(err, ErrDraining) {
		t.Fatalf("停机后提交应返回 ErrDraining，实际 %v", err)
	}
}

func TestGracefulStopDrainsInflight(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	var finished atomic.Int32
	for i := 0; i < 10; i++ {
		_, _, _ = a.sched.Submit(JobSpec{ID: fmt.Sprintf("d%d", i), Tenant: "t", Kind: KindNoop,
			Req: Resources{CPU: 1, Mem: 1 << 20},
			Run: func(ctx context.Context, j *Job) error {
				select {
				case <-time.After(100 * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
				finished.Add(1)
				return nil
			}})
	}
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	a.run.GracefulStop(a.sched, 5*time.Second)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("排空超时，用时 %s", elapsed)
	}
	if a.run.ActiveCount() != 0 {
		t.Fatalf("排空后不应有在途任务，实际 %d", a.run.ActiveCount())
	}
	// 资源必须全部归还。
	if u := a.pool.Used(); u != (Resources{}) {
		t.Fatalf("排空后资源应全部释放，实际 %s", u)
	}
}

func TestGracefulStopForcesCancelOnTimeout(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	var sawCancel atomic.Bool
	_, _, _ = a.sched.Submit(JobSpec{ID: "stuck", Tenant: "t", Kind: KindNoop,
		Req: Resources{CPU: 1, Mem: 1 << 20},
		Run: func(ctx context.Context, j *Job) error {
			select {
			case <-time.After(30 * time.Second):
				return nil
			case <-ctx.Done():
				sawCancel.Store(true)
				return ctx.Err()
			}
		}})
	time.Sleep(80 * time.Millisecond)

	a.run.GracefulStop(a.sched, 300*time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !sawCancel.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	if !sawCancel.Load() {
		t.Fatal("排空超时后应强制取消残留任务")
	}
}

func TestGracefulStopFlushesWAL(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	_, _, _ = a.sched.Submit(JobSpec{ID: "w", Tenant: "t", Kind: KindNoop,
		Req: Resources{CPU: 1, Mem: 1 << 20}, Run: noopTask})
	a.run.GracefulStop(a.sched, 2*time.Second)
	// Flush 后数据必须可重放（不依赖进程退出）。
	recs, _, err := Replay(a.wal.Path())
	if err != nil {
		t.Fatalf("停机后重放失败: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("停机落盘后应能重放出作业")
	}
}

// ---- 9. 状态机 ----------------------------------------------------------

func TestStateMachineRejectsIllegalTransitions(t *testing.T) {
	j := &Job{ID: "x", State: StatePending, done: make(chan struct{})}
	if err := j.transition(StateRunning); err != nil {
		t.Fatalf("pending->running 应合法: %v", err)
	}
	if err := j.transition(StateSucceeded); err != nil {
		t.Fatalf("running->succeeded 应合法: %v", err)
	}
	if err := j.transition(StateRunning); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("终态不应可离开，实际 err=%v", err)
	}
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []JobState{StateSucceeded, StateFailed, StateCanceled, StateRejected} {
		if !s.Terminal() {
			t.Fatalf("%s 应为终态", s)
		}
	}
	for _, s := range []JobState{StatePending, StateRunning} {
		if s.Terminal() {
			t.Fatalf("%s 不应为终态", s)
		}
	}
}

func TestPanicInTaskReleasesResources(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	_, _, _ = a.sched.Submit(JobSpec{ID: "panicky", Tenant: "t", Kind: KindNoop, MaxAttempts: 1,
		Req: Resources{CPU: 4, Mem: 1 << 30, GPU: 2},
		Run: func(ctx context.Context, j *Job) error { panic("算子炸了") }})
	waitState(t, a.sched, "panicky", StateFailed, 3*time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && a.pool.Used() != (Resources{}) {
		time.Sleep(10 * time.Millisecond)
	}
	if u := a.pool.Used(); u != (Resources{}) {
		t.Fatalf("任务 panic 后资源必须归还（防止泄漏），实际仍占用 %s", u)
	}
}

// ---- 10. 并发安全 -------------------------------------------------------

func TestConcurrentSubmissionsAreSafe(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			a.sched.Submit(JobSpec{
				ID: fmt.Sprintf("c%d", i), Tenant: fmt.Sprintf("t%d", i%8), Kind: KindNoop,
				Req: Resources{CPU: 1, Mem: 1 << 20},
				Run: sleepTask(5 * time.Millisecond),
			})
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := a.sched.Status()
		if st.Pending == 0 && st.Running == 0 && st.Unfit == 0 && st.RetryQueue == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st := a.sched.Status()
	if st.Succeeded != n {
		t.Fatalf("应全部成功 %d 个，实际成功 %d（排队=%d 运行=%d 暂存=%d）",
			n, st.Succeeded, st.Pending, st.Running, st.Unfit)
	}
	if u := a.pool.Used(); u != (Resources{}) {
		t.Fatalf("全部完成后资源应释放，实际 %s", u)
	}
}

// heapPushAll 便于构造堆（测试辅助）。
func heapPushAll(rq *readyQueue, items ...*readyItem) {
	for _, it := range items {
		heap.Push(rq, it)
	}
}

func TestNoResourceLeakAcrossManyJobs(t *testing.T) {
	a, done := newTestApp(t, smallPool())
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackground(ctx)

	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("leak-%d", i)
		_, _, _ = a.sched.Submit(JobSpec{
			ID: id, Tenant: fmt.Sprintf("t%d", i%5), Kind: KindNoop,
			Req: Resources{CPU: 2, Mem: 1 << 30, GPU: 1},
			Run: func(ctx context.Context, j *Job) error {
				if strings.HasSuffix(j.ID, "7") {
					return errors.New("偶发失败")
				}
				return nil
			}})
	}
	waitForQuiesce(t, a.sched, 15*time.Second)
	if u := a.pool.Used(); u != (Resources{}) {
		t.Fatalf("全部作业结束后资源应释放，实际 %s", u)
	}
	st := a.pool.Stats()
	if st.Reserve != st.Release {
		t.Fatalf("预留与释放次数应相等：reserve=%d release=%d", st.Reserve, st.Release)
	}
}

func waitForQuiesce(t *testing.T, s *Scheduler, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := s.Status()
		if st.Pending == 0 && st.Running == 0 && st.Unfit == 0 && st.RetryQueue == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("调度器在超时内未静默")
}
