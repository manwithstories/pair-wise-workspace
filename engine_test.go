package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// newTestEngine 造一个干净引擎（独立 WAL 文件、无日志、毫秒级 tick）。
func newTestEngine(t *testing.T, mutate func(*EngineOptions)) *Engine {
	t.Helper()
	opts := EngineOptions{
		WALPath:     fmt.Sprintf("%s/e-%d.wal", t.TempDir(), time.Now().UnixNano()),
		PoolTotal:   Resources{CPU: 64, MemMB: 256 * 1024, GPU: 8},
		Tick:        2 * time.Millisecond,
		DrainBudget: 5 * time.Second,
	}
	if mutate != nil {
		mutate(&opts)
	}
	e, err := NewEngine(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.Shutdown(context.Background()) })
	return e
}

// specWork 返回一个遵守 WorkSpec 的执行体：按单元推进、支持脚本化失败、
// 收到取消信号时立刻返回并且丢弃已累积的部分结果。
//
// 它就是生产用的 defaultWorkFunc/demoWorkFunc 的等价物——测试必须走同一条
// 路径，否则「取消要丢弃部分结果」「失败要能重试」这些行为根本没被验证到。
func specWork() RunFunc { return defaultWorkFunc }

// fastWork 返回一个固定时长的执行体，用于只关心时序的场景。
func fastWork(d time.Duration) RunFunc {
	return func(ctx context.Context, job *Job) (Result, error) {
		select {
		case <-ctx.Done():
			// 取消路径丢弃部分结果，不外泄。
			return Result{}, ctx.Err()
		case <-time.After(d):
		}
		if err := job.transition(StateSucceeded, "test"); err != nil {
			return Result{}, err
		}
		return Result{JobKey: job.IdemKey, Items: 1}, nil
	}
}

// settled 判断某批作业是否已经彻底结算完毕：既没有待调度/在途作业，
// 资源也已经全部归还（归还发生在执行体 cleanup 里，晚于 InFlight 归零，
// 所以必须显式检查资源池，否则会读到「计数已归零但资源还没还」的中间态）。
//
// 注意不能用 len(GroupNames())==0 作为判据：已结算完的作业组会保留在索引里
// 直到排空阶段才退休，用它判断会让测试永远等不到条件成立。
func (e *Engine) settled() bool {
	return e.sched.PendingCount() == 0 &&
		e.runner.InFlight() == 0 &&
		e.pool.Allocated() == (Resources{}) &&
		e.sched.noTenantReservations()
}

// waitIdle 等引擎结算完毕。
func waitIdle(t *testing.T, e *Engine, d time.Duration, msg string) {
	t.Helper()
	waitFor(t, d, e.settled, msg)
}

// waitFor 轮询等待条件成立。
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// ---------------------------------------------------------------------------
// 幂等与并发提交
// ---------------------------------------------------------------------------

func TestEngineIdempotentSubmitReturnsSameHandle(t *testing.T) {
	e := newTestEngine(t, func(o *EngineOptions) { o.WorkFunc = fastWork(50 * time.Millisecond) })
	e.Start()
	ctx := context.Background()
	req := SubmitRequest{Tenant: "cv", IdemKey: "k1", Want: Resources{CPU: 2, MemMB: 64}, Work: WorkSpec{Units: 1}}

	j1, created1, err := e.Submit(ctx, req)
	if err != nil || !created1 {
		t.Fatalf("first: created=%v err=%v", created1, err)
	}
	j2, created2, err := e.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if created2 || j1 != j2 {
		t.Fatal("duplicate submit must return the same handle and not re-create")
	}
}

// 100 并发提交同一个 key：只有一次入队成功，其余全部拿到同一句柄。
func TestEngineConcurrentDuplicateSubmit(t *testing.T) {
	e := newTestEngine(t, func(o *EngineOptions) { o.WorkFunc = fastWork(20 * time.Millisecond) })
	e.Start()
	ctx := context.Background()
	req := SubmitRequest{Tenant: "cv", IdemKey: "same",
		Want: Resources{CPU: 1, MemMB: 32}, Work: WorkSpec{Units: 1}}

	var wg sync.WaitGroup
	var mu sync.Mutex
	handles := map[*Job]int{}
	created := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, ok, err := e.Submit(ctx, req)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			handles[j]++
			if ok {
				created++
			}
		}()
	}
	wg.Wait()

	if created != 1 {
		t.Fatalf("exactly one submit should create the job, got %d", created)
	}
	if len(handles) != 1 {
		t.Fatalf("all callers should share one handle, got %d distinct", len(handles))
	}
	for h, n := range handles {
		if n != 100 {
			t.Fatalf("handle returned %d times, want 100 (handle %v)", n, h)
		}
	}
}

// ---------------------------------------------------------------------------
// 账本一致性
// ---------------------------------------------------------------------------

// 所有作业跑完后，两边账本都必须归零：这是 DRF 正确性的前提。
func TestEngineLedgerReturnsToZero(t *testing.T) {
	e := newTestEngine(t, func(o *EngineOptions) { o.WorkFunc = fastWork(10 * time.Millisecond) })
	e.Start()
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		if _, _, err := e.Submit(ctx, SubmitRequest{
			Tenant: fmt.Sprintf("t%d", i%4), IdemKey: fmt.Sprintf("j%d", i),
			Want: Resources{CPU: 1, MemMB: 32, GPU: 0}, Work: WorkSpec{Units: 1},
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitIdle(t, e, 10*time.Second, "all jobs to finish")

	if got := e.pool.Available(); got != e.pool.Total() {
		t.Fatalf("pool not fully returned: %s", got)
	}
	for _, sh := range e.sched.Shares(e.pool) {
		if sh.Usage != (Resources{}) {
			t.Fatalf("tenant %s still holds %s after all jobs finished", sh.Tenant, sh.Usage)
		}
	}
}

// 反复执行必须始终账本平衡（回归保护：曾经出现过 reserved 涨到 2 倍池容量）。
func TestEngineLedgerStableAcrossRounds(t *testing.T) {
	e := newTestEngine(t, func(o *EngineOptions) { o.WorkFunc = fastWork(5 * time.Millisecond) })
	e.Start()
	ctx := context.Background()
	for round := 0; round < 3; round++ {
		for i := 0; i < 20; i++ {
			if _, _, err := e.Submit(ctx, SubmitRequest{
				Tenant: fmt.Sprintf("t%d", i%3), IdemKey: fmt.Sprintf("r%d-j%d", round, i),
				Want: Resources{CPU: 2, MemMB: 64, GPU: 0}, Work: WorkSpec{Units: 1},
			}); err != nil {
				t.Fatal(err)
			}
		}
		waitIdle(t, e, 10*time.Second, fmt.Sprintf("round %d to finish", round))
		if got := e.pool.Allocated(); got != (Resources{}) {
			t.Fatalf("round %d: pool leaked %s", round, got)
		}
		for _, sh := range e.sched.Shares(e.pool) {
			if sh.Share > 1.0 {
				t.Fatalf("round %d: tenant %s share %.3f exceeds pool capacity (oversubscription)",
					round, sh.Tenant, sh.Share)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 依赖与 errgroup
// ---------------------------------------------------------------------------

func TestEngineUpstreamFailureCancelsDownstream(t *testing.T) {
	// 用真实的 specWork，才能触发脚本化失败与依赖取消。
	e := newTestEngine(t, func(o *EngineOptions) { o.WorkFunc = specWork() })
	e.Start()
	ctx := context.Background()
	if _, _, err := e.Submit(ctx, SubmitRequest{
		Tenant: "d", IdemKey: "up", Group: "g", Want: Resources{CPU: 1, MemMB: 32},
		Work: WorkSpec{Units: 1, UnitDuration: 20 * time.Millisecond, FailAlways: true},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := e.Submit(ctx, SubmitRequest{
			Tenant: "d", IdemKey: fmt.Sprintf("down%d", i), Group: "g",
			DepRefs: []DepRef{{IdemKey: "up"}}, Want: Resources{CPU: 1, MemMB: 32},
			Work: WorkSpec{Units: 4, UnitDuration: 100 * time.Millisecond},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 等待依赖链完全结算：三个下游必须都离开活跃索引并被判定为终态。
	// 不能只等引擎空闲——上游失败的瞬间下游可能还没跑过依赖判定，
	// 此时 pending/inFlight/资源都已经归零，空闲条件会提前成立。
	waitFor(t, 10*time.Second, func() bool {
		for i := 0; i < 3; i++ {
			if _, ok := e.sched.Lookup(fmt.Sprintf("down%d", i)); ok {
				return false
			}
		}
		return e.runner.InFlight() == 0 && e.pool.Allocated() == (Resources{})
	}, "dependency chain to be canceled")
	waitIdle(t, e, 5*time.Second, "resources to return")

	// 三条下游的取消决策可能分布在不同轮次里落 WAL（有的在依赖扫描时写，
	// 有的在提交阶段同步写），这里等到引擎完全静止后再读，避免读到半截状态。
	// WAL 是这里的事实来源：轮询直到上游的 failed 与三个下游的 canceled
	// 全部落盘。取消事件分布在不同轮次写入（有的在依赖扫描、有的在提交阶段），
	// 任何基于内存状态的判据都会读到中间态。
	canceled := map[string]bool{}
	failed := map[string]bool{}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_ = e.wal.Flush() // 非 durable 事件可能还在写缓冲里
		recs, _, err := readWALFile(e.wal.Path())
		if err != nil {
			t.Fatal(err)
		}
		canceled = map[string]bool{}
		failed = map[string]bool{}
		for _, r := range recs {
			switch r.Kind {
			case evCanceled:
				canceled[r.IdemKey] = true
			case evFailed:
				failed[r.IdemKey] = true
			}
		}
		all := failed["up"] && canceled["down0"] && canceled["down1"] && canceled["down2"]
		if all || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitIdle(t, e, 5*time.Second, "engine to go fully quiet")
	if !failed["up"] {
		t.Error("upstream should have failed")
	}
	for i := 0; i < 3; i++ {
		k := fmt.Sprintf("down%d", i)
		if !canceled[k] {
			t.Errorf("%s should have been canceled after upstream failure", k)
		}
	}
	if got := e.pool.Available(); got != e.pool.Total() {
		t.Fatalf("canceled chain leaked resources: %s", got)
	}
}

// ---------------------------------------------------------------------------
// 重试
// ---------------------------------------------------------------------------

func TestEngineRetriesWithBackoffThenSucceeds(t *testing.T) {
	e := newTestEngine(t, func(o *EngineOptions) { o.WorkFunc = specWork() })
	e.runner.SetBackoff(10*time.Millisecond, 40*time.Millisecond)
	e.Start()
	if _, _, err := e.Submit(context.Background(), SubmitRequest{
		Tenant: "rec", IdemKey: "flaky", Want: Resources{CPU: 1, MemMB: 32},
		MaxRetries: 3, Work: WorkSpec{Units: 1, UnitDuration: 10 * time.Millisecond, FailFirst: 2},
	}); err != nil {
		t.Fatal(err)
	}
	// 等待条件必须是「作业真的跑完并离开活跃索引」。
	// 用引擎空闲（settled）会过早返回：第一次失败后作业会短暂处于
	// pending（退避中），而资源已归还，settled() 会在退避计时器触发前
	// 就判定为真，于是统计到的 successes 还是 0。
	waitFor(t, 10*time.Second, func() bool {
		_, ok := e.sched.Lookup("flaky")
		return !ok && e.runner.InFlight() == 0
	}, "flaky job to leave the active index")
	waitIdle(t, e, 5*time.Second, "flaky job resources to return")

	st := e.runner.Stats()
	if st.Retries < 2 {
		t.Fatalf("retries = %d, want >= 2 (FailFirst=2)", st.Retries)
	}
	if st.Successes < 1 {
		t.Fatalf("successes = %d, want >= 1", st.Successes)
	}
	if st.Failures != 0 {
		t.Fatalf("failures = %d, want 0 (it should succeed on the 3rd attempt)", st.Failures)
	}
	if got := e.pool.Available(); got != e.pool.Total() {
		t.Fatalf("retry path leaked resources: %s", got)
	}
}

// ---------------------------------------------------------------------------
// 优雅停机
// ---------------------------------------------------------------------------

func TestEngineShutdownDrainsAndReturnsResources(t *testing.T) {
	e := newTestEngine(t, func(o *EngineOptions) { o.WorkFunc = fastWork(200 * time.Millisecond) })
	e.Start()
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		if _, _, err := e.Submit(ctx, SubmitRequest{
			Tenant: "cv", IdemKey: fmt.Sprintf("j%d", i), Group: "g",
			Want: Resources{CPU: 2, MemMB: 64}, Work: WorkSpec{Units: 1},
		}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(80 * time.Millisecond) // 让一些进入 running

	clean, detail := e.Shutdown(context.Background())
	if !clean {
		t.Fatalf("expected a clean drain, got detail=%q", detail)
	}
	if got := e.pool.Available(); got != e.pool.Total() {
		t.Fatalf("shutdown must return every resource, got %s", got)
	}
	// 停机后不再接受新提交。
	if _, _, err := e.Submit(context.Background(), SubmitRequest{
		Tenant: "cv", IdemKey: "late", Want: Resources{CPU: 1, MemMB: 32},
	}); err != ErrDraining {
		t.Fatalf("post-shutdown submit should be rejected, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// WAL 恢复
// ---------------------------------------------------------------------------

// 崩溃后在途作业必须被重放重建，且能继续跑完。
func TestEngineRecoversInFlightJobsFromWAL(t *testing.T) {
	dir := t.TempDir()
	walPath := dir + "/crash.wal"
	total := Resources{CPU: 64, MemMB: 256 * 1024, GPU: 8}

	// 第一阶段：提交后在途「崩溃」。
	e1, err := NewEngine(EngineOptions{
		WALPath: walPath, PoolTotal: total, Tick: 2 * time.Millisecond,
		WorkFunc: fastWork(500 * time.Millisecond), // 长作业，保证崩溃时还在跑
	})
	if err != nil {
		t.Fatal(err)
	}
	e1.Start()
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if _, _, err := e1.Submit(ctx, SubmitRequest{
			Tenant: "cv", IdemKey: fmt.Sprintf("j%d", i), Want: Resources{CPU: 2, MemMB: 64},
			Work: WorkSpec{Units: 1},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 重复提交一次，验证重放不会把它变成两个。
	if _, _, err := e1.Submit(ctx, SubmitRequest{
		Tenant: "cv", IdemKey: "j0", Want: Resources{CPU: 2, MemMB: 64}, Work: WorkSpec{Units: 1},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	inFlightBefore := e1.runner.InFlight()
	e1.wal.hardCloseForCrash() // 模拟 SIGKILL：不 Flush 不 Close

	if inFlightBefore == 0 {
		t.Fatal("expected jobs to be in flight before the simulated crash")
	}

	// 第二阶段：新引擎从同一 WAL 恢复。
	e2, err := NewEngine(EngineOptions{
		WALPath: walPath, PoolTotal: total, Tick: 2 * time.Millisecond,
		WorkFunc: fastWork(10 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = e2.Shutdown(context.Background()) }()

	if e2.RecoveredCount() != 4 {
		t.Fatalf("recovered %d jobs, want 4 (duplicate submit must not duplicate)", e2.RecoveredCount())
	}
	if got := e2.sched.PendingCount(); got != 4 {
		t.Fatalf("pending after recovery = %d, want 4", got)
	}
	e2.Start()
	waitIdle(t, e2, 10*time.Second, "recovered jobs to finish")
	if got := e2.runner.Stats().Successes; got != 4 {
		t.Fatalf("recovered jobs succeeded = %d, want 4", got)
	}
	if got := e2.pool.Available(); got != e2.pool.Total() {
		t.Fatalf("recovery leaked resources: %s", got)
	}
}

// 依赖关系必须在重放后依然成立。
func TestEngineRecoveryPreservesDependencies(t *testing.T) {
	dir := t.TempDir()
	walPath := dir + "/dep.wal"
	total := Resources{CPU: 64, MemMB: 64 * 1024, GPU: 4}

	e1, err := NewEngine(EngineOptions{
		WALPath: walPath, PoolTotal: total, Tick: 2 * time.Millisecond,
		WorkFunc: fastWork(500 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	e1.Start()
	ctx := context.Background()
	e1.Submit(ctx, SubmitRequest{Tenant: "d", IdemKey: "up",
		Want: Resources{CPU: 1, MemMB: 32}, Work: WorkSpec{Units: 1}})
	e1.Submit(ctx, SubmitRequest{Tenant: "d", IdemKey: "down",
		DepRefs: []DepRef{{IdemKey: "up"}},
		Want:    Resources{CPU: 1, MemMB: 32}, Work: WorkSpec{Units: 1}})
	time.Sleep(60 * time.Millisecond)
	e1.wal.hardCloseForCrash()

	e2, err := NewEngine(EngineOptions{
		WALPath: walPath, PoolTotal: total, Tick: 2 * time.Millisecond,
		WorkFunc: fastWork(10 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 这个用例只验证「重放保住了依赖关系」，不需要真正跑起来，
	// 因此不调用 Start()；Shutdown 依赖 drainer（由 Start 拉起），
	// 这里直接关 WAL 收尾即可。
	defer e2.wal.Close()

	if e2.RecoveredCount() != 2 {
		t.Fatalf("recovered %d, want 2", e2.RecoveredCount())
	}
	down, ok := e2.sched.Lookup("down")
	if !ok {
		t.Fatal("downstream job not recovered")
	}
	if len(down.DepRefs) != 1 || down.DepRefs[0].IdemKey != "up" {
		t.Fatalf("dependency lost across recovery: %+v", down.DepRefs)
	}
}

// 已经完成的作业绝不能在重启后被重放成待调度——否则每次重启都会把
// 跑完的训练/评测再烧一遍卡。
func TestEngineRecoverySkipsCompletedJobs(t *testing.T) {
	walPath := t.TempDir() + "/done.wal"
	total := Resources{CPU: 64, MemMB: 64 * 1024, GPU: 4}

	e1, err := NewEngine(EngineOptions{
		WALPath: walPath, PoolTotal: total, Tick: 2 * time.Millisecond,
		WorkFunc: fastWork(10 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	e1.Start()
	ctx := context.Background()
	if _, _, err := e1.Submit(ctx, SubmitRequest{
		Tenant: "cv", IdemKey: "done-1", Want: Resources{CPU: 1, MemMB: 32},
		Work: WorkSpec{Units: 1},
	}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e1, 10*time.Second, "job to complete")
	if got := e1.runner.Stats().Successes; got != 1 {
		t.Fatalf("successes = %d, want 1", got)
	}
	// 优雅停机，确保 WAL 里落下 completed 事件。
	if clean, detail := e1.Shutdown(context.Background()); !clean {
		t.Fatalf("shutdown not clean: %s", detail)
	}

	e2, err := NewEngine(EngineOptions{
		WALPath: walPath, PoolTotal: total, Tick: 2 * time.Millisecond,
		WorkFunc: fastWork(10 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e2.wal.Close()

	if n := e2.RecoveredCount(); n != 0 {
		t.Fatalf("recovered %d jobs, want 0 (completed jobs must not be replayed)", n)
	}
	if got := e2.sched.PendingCount(); got != 0 {
		t.Fatalf("pending after recovery = %d, want 0", got)
	}
	// 但终态记忆必须保留，好让下游依赖判定知道「上游已经成功」。
	e2.sched.ResolveDependencies()
}

// 下游作业在重启后仍应识别出「上游已成功」，而不是永远等待。
func TestEngineRecoveryKeepsUpstreamTerminalState(t *testing.T) {
	walPath := t.TempDir() + "/term.wal"
	total := Resources{CPU: 64, MemMB: 64 * 1024, GPU: 4}

	e1, err := NewEngine(EngineOptions{
		WALPath: walPath, PoolTotal: total, Tick: 2 * time.Millisecond,
		WorkFunc: fastWork(10 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	e1.Start()
	ctx := context.Background()
	e1.Submit(ctx, SubmitRequest{Tenant: "d", IdemKey: "up",
		Want: Resources{CPU: 1, MemMB: 32}, Work: WorkSpec{Units: 1}})
	e1.Submit(ctx, SubmitRequest{Tenant: "d", IdemKey: "down",
		DepRefs: []DepRef{{IdemKey: "up"}},
		Want:    Resources{CPU: 1, MemMB: 32}, Work: WorkSpec{Units: 1}})
	waitIdle(t, e1, 10*time.Second, "both jobs to complete")
	if clean, detail := e1.Shutdown(context.Background()); !clean {
		t.Fatalf("shutdown not clean: %s", detail)
	}

	e2, err := NewEngine(EngineOptions{
		WALPath: walPath, PoolTotal: total, Tick: 2 * time.Millisecond,
		WorkFunc: fastWork(10 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e2.wal.Close()
	if got := e2.RecoveredCount(); got != 0 {
		t.Fatalf("recovered %d, want 0 (chain already finished)", got)
	}
	if got := e2.sched.PendingCount(); got != 0 {
		t.Fatalf("pending = %d, want 0 (downstream must not wait forever on a finished upstream)", got)
	}
}
