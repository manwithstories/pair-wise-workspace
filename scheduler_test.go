package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 资源池
// ---------------------------------------------------------------------------

func TestPoolAcquireReleaseIsBalanced(t *testing.T) {
	p, err := NewPool(Resources{CPU: 64, MemMB: 256 * 1024, GPU: 8})
	if err != nil {
		t.Fatal(err)
	}
	want := Resources{CPU: 8, MemMB: 16 * 1024, GPU: 1}
	if err := p.Acquire("cv", "j1", want); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := p.Allocated(); got != want {
		t.Fatalf("allocated = %s, want %s", got, want)
	}
	if got := p.TenantUsage("cv"); got != want {
		t.Fatalf("tenant usage = %s, want %s", got, want)
	}
	p.Release("cv", "j1", want)
	if got := p.Allocated(); got != (Resources{}) {
		t.Fatalf("after release allocated = %s, want zero", got)
	}
}

// 重复 Release 必须幂等，否则取消路径重复触发会把账本减成负数（超卖）。
func TestPoolReleaseIsIdempotent(t *testing.T) {
	p, _ := NewPool(Resources{CPU: 64, MemMB: 1024, GPU: 4})
	want := Resources{CPU: 4, MemMB: 128, GPU: 1}
	if err := p.Acquire("a", "k", want); err != nil {
		t.Fatal(err)
	}
	p.Release("a", "k", want)
	p.Release("a", "k", want)
	p.Release("a", "k", want)
	if got := p.Allocated(); got != (Resources{}) {
		t.Fatalf("double release corrupted ledger: %s", got)
	}
}

// 三维必须整体满足：只要有一维放不下就整体拒绝，不允许「卡扣了内存没扣」。
func TestPoolAcquireIsAllOrNothing(t *testing.T) {
	p, _ := NewPool(Resources{CPU: 8, MemMB: 1024, GPU: 2})
	if err := p.Acquire("a", "k1", Resources{CPU: 8, MemMB: 1024, GPU: 2}); err != nil {
		t.Fatal(err)
	}
	err := p.Acquire("a", "k2", Resources{CPU: 1, MemMB: 1, GPU: 1})
	if !errors.Is(err, ErrInsufficient) {
		t.Fatalf("want ErrInsufficient, got %v", err)
	}
	if got := p.Allocated(); got != (Resources{CPU: 8, MemMB: 1024, GPU: 2}) {
		t.Fatalf("partial acquire leaked: %s", got)
	}
	err = p.Acquire("a", "k3", Resources{CPU: 999, MemMB: 1, GPU: 0})
	if err == nil || errors.Is(err, ErrInsufficient) {
		t.Fatalf("oversized request should be a hard error, got %v", err)
	}
}

func TestPoolConcurrentAcquireRelease(t *testing.T) {
	p, _ := NewPool(Resources{CPU: 64, MemMB: 64 * 1024, GPU: 8})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("j%d", i)
			want := Resources{CPU: 1, MemMB: 64, GPU: 0}
			if err := p.Acquire("t", key, want); err != nil {
				return
			}
			p.Release("t", key, want)
		}(i)
	}
	wg.Wait()
	if got := p.Allocated(); got != (Resources{}) {
		t.Fatalf("ledger not balanced after concurrency: %s", got)
	}
}

// ---------------------------------------------------------------------------
// DRF
// ---------------------------------------------------------------------------

func TestDominantShareIgnoresZeroDimensions(t *testing.T) {
	total := Resources{CPU: 64, MemMB: 1024, GPU: 8}
	// 只用内存的作业：主导份额是内存比例，不被零 GPU 拉低。
	if got := dominantShare(Resources{MemMB: 512}, total); got != 0.5 {
		t.Fatalf("dominantShare = %v, want 0.5", got)
	}
	if got := dominantShare(Resources{}, total); got != 0 {
		t.Fatalf("empty usage share = %v, want 0", got)
	}
}

func TestSubmitDeduplicatesByIdempotencyKey(t *testing.T) {
	s := NewScheduler(schedulerOptions{})
	req := SubmitRequest{Tenant: "cv", IdemKey: "train-1",
		Want: Resources{CPU: 8, GPU: 2}, Work: WorkSpec{Units: 1}}
	j1, created1, err := s.Submit(req)
	if err != nil || !created1 {
		t.Fatalf("first submit: created=%v err=%v", created1, err)
	}
	j2, created2, err := s.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	if created2 {
		t.Fatal("duplicate submit must not create a second job")
	}
	if j1 != j2 {
		t.Fatal("duplicate submit must return the same handle")
	}
	if got := s.Stats().Active; got != 1 {
		t.Fatalf("active = %d, want 1 (no double enqueue)", got)
	}
}

func TestSubmitRejectsCrossTenantKeyReuse(t *testing.T) {
	s := NewScheduler(schedulerOptions{})
	if _, _, err := s.Submit(SubmitRequest{Tenant: "cv", IdemKey: "k", Want: Resources{CPU: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Submit(SubmitRequest{Tenant: "rec", IdemKey: "k", Want: Resources{CPU: 1}}); err == nil {
		t.Fatal("cross-tenant key reuse must be rejected")
	}
}

// 池被占满后，排在前面的作业塞不下时必须退到能塞下的候选，
// 而不是反复选中同一个塞不下的作业把整个池卡死。
func TestPlanSkipsOversizedCandidate(t *testing.T) {
	pool, _ := NewPool(Resources{CPU: 8, MemMB: 1024, GPU: 2})
	s := NewScheduler(schedulerOptions{})
	// 两个作业请求相同（都要满池），只有一条能拿到。
	s.Submit(SubmitRequest{Tenant: "cv", IdemKey: "full-1",
		Want: Resources{CPU: 8, MemMB: 1024, GPU: 2}, Work: WorkSpec{Units: 1}})
	s.Submit(SubmitRequest{Tenant: "cv", IdemKey: "full-2",
		Want: Resources{CPU: 8, MemMB: 1024, GPU: 2}, Work: WorkSpec{Units: 1}})

	now := time.Now()
	p1, _ := s.Plan(pool, now)
	if p1 == nil {
		t.Fatal("first plan returned nothing")
	}
	if s.Acquire(pool, p1) == nil {
		t.Fatal("first acquire should succeed")
	}
	// 池已满：第二次决策必须报告塞不下，而不是空转。
	p2, se := s.Plan(pool, now)
	if p2 != nil {
		t.Fatalf("expected no candidate on a full pool, got %s", p2.Job.IdemKey)
	}
	if !se.NoFit {
		t.Fatalf("selectErrors should report NoFit, got %+v", se)
	}
}

// 主导份额最低的租户应优先被调度。
func TestPlanPrefersLowestDominantShare(t *testing.T) {
	pool, _ := NewPool(Resources{CPU: 64, MemMB: 1024, GPU: 8})
	s := NewScheduler(schedulerOptions{})
	// cv 先占住一半内存。
	s.Submit(SubmitRequest{Tenant: "cv", IdemKey: "cv-1",
		Want: Resources{CPU: 1, MemMB: 512}, Work: WorkSpec{Units: 1}})
	p1, _ := s.Plan(pool, time.Now())
	s.Acquire(pool, p1)
	// nlp 与 rec 各提交一个同样大小的作业。
	s.Submit(SubmitRequest{Tenant: "nlp", IdemKey: "nlp-1",
		Want: Resources{CPU: 1, MemMB: 32}, Work: WorkSpec{Units: 1}})
	s.Submit(SubmitRequest{Tenant: "rec", IdemKey: "rec-1",
		Want: Resources{CPU: 1, MemMB: 32}, Work: WorkSpec{Units: 1}})

	p2, _ := s.Plan(pool, time.Now())
	if p2 == nil {
		t.Fatal("expected a pick")
	}
	if p2.Job.Tenant == "cv" {
		t.Fatalf("picked cv (share 0.5) over idle tenants; picked=%s", p2.Job.IdemKey)
	}
}

// ---------------------------------------------------------------------------
// 状态机与记账
// ---------------------------------------------------------------------------

func TestJobStateMachineRejectsIllegalTransition(t *testing.T) {
	j := newJob(0, "cv", "k", Resources{CPU: 1}, nil, WorkSpec{}, 0)
	if err := j.transition(StateSucceeded, "skip running"); err == nil {
		t.Fatal("pending->succeeded must be rejected")
	}
	if err := j.transition(StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := j.transition(StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if !j.State().Terminal() {
		t.Fatal("succeeded must be terminal")
	}
}

// 跑完的作业必须把租户账本还回去，否则 DRF 份额会累积成超卖。
func TestFinishReleasesTenantReservation(t *testing.T) {
	pool, _ := NewPool(Resources{CPU: 64, MemMB: 1024, GPU: 8})
	s := NewScheduler(schedulerOptions{})
	s.Submit(SubmitRequest{Tenant: "cv", IdemKey: "k",
		Want: Resources{CPU: 4, MemMB: 64, GPU: 1}, Work: WorkSpec{Units: 1}})
	p, _ := s.Plan(pool, time.Now())
	if s.Acquire(pool, p) == nil {
		t.Fatal("acquire failed")
	}
	if got := s.TenantReserved("cv"); got != p.Job.Want {
		t.Fatalf("reserved = %s, want %s", got, p.Job.Want)
	}
	s.Finish(p.Job, StateSucceeded, nil)
	if got := s.TenantReserved("cv"); got != (Resources{}) {
		t.Fatalf("reserved after finish = %s, want zero", got)
	}
	// 注意：Finish 只负责调度器侧的租户账本，资源池的归还由 runner 的
	// cleanup 路径负责（它知道执行体实际跑了多久、持有了多少资源）。
	// 这里模拟 runner 归还资源，确认两边账本最终对齐。
	pool.Release(p.Job.Tenant, p.Job.IdemKey, p.Job.Want)
	if got := pool.Available(); got != pool.Total() {
		t.Fatalf("pool not fully returned: %s", got)
	}
}

// 幂等：重复 Finish 不应把账本减成负数。
func TestFinishIsIdempotent(t *testing.T) {
	pool, _ := NewPool(Resources{CPU: 64, MemMB: 1024, GPU: 8})
	s := NewScheduler(schedulerOptions{})
	s.Submit(SubmitRequest{Tenant: "cv", IdemKey: "k",
		Want: Resources{CPU: 4, MemMB: 64}, Work: WorkSpec{Units: 1}})
	p, _ := s.Plan(pool, time.Now())
	s.Acquire(pool, p)
	s.Finish(p.Job, StateSucceeded, nil)
	s.Finish(p.Job, StateCanceled, errors.New("late cancel"))
	if got := s.TenantReserved("cv"); got != (Resources{}) {
		t.Fatalf("double finish corrupted ledger: %s", got)
	}
}

// ---------------------------------------------------------------------------
// 依赖
// ---------------------------------------------------------------------------

func TestDownstreamCanceledWhenUpstreamFails(t *testing.T) {
	pool, _ := NewPool(Resources{CPU: 8, MemMB: 1024, GPU: 2})
	s := NewScheduler(schedulerOptions{})
	s.Submit(SubmitRequest{Tenant: "d", IdemKey: "up", Want: Resources{CPU: 1}, Work: WorkSpec{Units: 1}})
	s.Submit(SubmitRequest{Tenant: "d", IdemKey: "down", Want: Resources{CPU: 1},
		DepRefs: []DepRef{{IdemKey: "up"}}, Work: WorkSpec{Units: 1}})

	// 上游 pending 时下游必须等待，不能被误判为失败。
	if got := s.ResolveDependencies(); len(got) != 0 {
		t.Fatalf("downstream must wait while upstream is pending, got %d", len(got))
	}

	// 上游被真正派发后失败。
	p1, _ := s.Plan(pool, time.Now())
	if s.Acquire(pool, p1) == nil {
		t.Fatal("acquire upstream failed")
	}
	s.Finish(p1.Job, StateFailed, errors.New("boom"))
	pool.Release(p1.Job.Tenant, p1.Job.IdemKey, p1.Job.Want)

	// 下游连带取消，不占用任何资源。
	changed := s.ResolveDependencies()
	if len(changed) != 1 || changed[0].State() != StateCanceled {
		t.Fatalf("downstream should be canceled, got %+v", changed)
	}
	if got := s.TenantReserved("d"); got != (Resources{}) {
		t.Fatalf("canceled downstream must not hold resources: %s", got)
	}
	if got := pool.Available(); got != pool.Total() {
		t.Fatalf("pool should be fully free, got %s", got)
	}
}

func TestDownstreamReleasedWhenUpstreamSucceeds(t *testing.T) {
	pool, _ := NewPool(Resources{CPU: 8, MemMB: 1024, GPU: 2})
	s := NewScheduler(schedulerOptions{})
	s.Submit(SubmitRequest{Tenant: "d", IdemKey: "up", Want: Resources{CPU: 1}, Work: WorkSpec{Units: 1}})
	s.Submit(SubmitRequest{Tenant: "d", IdemKey: "down", Want: Resources{CPU: 1},
		DepRefs: []DepRef{{IdemKey: "up"}}, Work: WorkSpec{Units: 1}})

	// 上游刚入队、还没拿到资源：下游必须继续等待，不能被判为「上游已死」。
	if got := s.ResolveDependencies(); len(got) != 0 {
		t.Fatalf("downstream must wait while upstream is pending, got %d changes", len(got))
	}

	// 真正派发并跑完上游。
	p1, _ := s.Plan(pool, time.Now())
	if p1 == nil || p1.Job.IdemKey != "up" {
		t.Fatalf("expected upstream to be scheduled first, got %v", p1)
	}
	if s.Acquire(pool, p1) == nil {
		t.Fatal("acquire upstream failed")
	}
	s.Finish(p1.Job, StateSucceeded, nil)
	pool.Release(p1.Job.Tenant, p1.Job.IdemKey, p1.Job.Want)

	// 上游成功后下游被唤醒，并可被调度。
	changed := s.ResolveDependencies()
	if len(changed) != 1 || changed[0].IdemKey != "down" {
		t.Fatalf("downstream should be woken, got %+v", changed)
	}
	pl, _ := s.Plan(pool, time.Now())
	if pl == nil || pl.Job.IdemKey != "down" {
		t.Fatalf("downstream should now be schedulable, got %v", pl)
	}
}

// ---------------------------------------------------------------------------
// 熔断器
// ---------------------------------------------------------------------------

func TestBreakerTripsAfterFiveFailuresAndHalfOpens(t *testing.T) {
	b := newTenantBreaker(60 * time.Second)
	now := time.Now()
	for i := 0; i < 4; i++ {
		if tripped, _ := b.Record(false, false, now); tripped {
			t.Fatalf("tripped too early at failure %d", i+1)
		}
	}
	tripped, state := b.Record(false, false, now)
	if !tripped || state != breakerOpen {
		t.Fatalf("5th failure should trip the breaker, got tripped=%v state=%v", tripped, state)
	}
	if ok, _ := b.Allow(now); ok {
		t.Fatal("open breaker must not allow jobs")
	}
	later := now.Add(61 * time.Second)
	if ok, reason := b.Allow(later); !ok {
		t.Fatalf("half-open should allow one probe: %s", reason)
	}
	if ok, _ := b.Allow(later); ok {
		t.Fatal("half-open must only allow a single probe")
	}
	if _, st := b.Record(true, false, later); st != breakerClosed {
		t.Fatalf("successful probe should close breaker, got %v", st)
	}
	if ok, _ := b.Allow(later); !ok {
		t.Fatal("closed breaker should allow jobs")
	}
}

// 取消不算租户的失败，不应推进熔断计数。
func TestBreakerIgnoresCancellations(t *testing.T) {
	b := newTenantBreaker(time.Minute)
	now := time.Now()
	for i := 0; i < 10; i++ {
		if tripped, _ := b.Record(false, true, now); tripped {
			t.Fatal("cancellations must not trip the breaker")
		}
	}
	if v := b.View(now); v.Consec != 0 {
		t.Fatalf("consecutive failures = %d, want 0", v.Consec)
	}
}

func TestBackoffGrowsExponentiallyAndCaps(t *testing.T) {
	base, cap_ := 100*time.Millisecond, time.Second
	want := []time.Duration{
		100 * time.Millisecond, 200 * time.Millisecond,
		400 * time.Millisecond, 800 * time.Millisecond, time.Second,
	}
	for i, w := range want {
		if got := backoff(base, i, cap_); got != w {
			t.Fatalf("backoff(%d) = %s, want %s", i, got, w)
		}
	}
}

// ---------------------------------------------------------------------------
// WAL
// ---------------------------------------------------------------------------

func TestWALRoundTrip(t *testing.T) {
	path := t.TempDir() + "/t.wal"
	w, _, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := w.Append(walRecord{Kind: evSubmitted, IdemKey: fmt.Sprintf("k%d", i),
			Tenant: "t", Want: Resources{CPU: 1, MemMB: 2, GPU: 0}}, true); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	w2, recs, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if !w2.Replayed() {
		t.Fatal("expected replayed=true")
	}
	if len(recs) != 20 {
		t.Fatalf("replayed %d records, want 20", len(recs))
	}
	if recs[19].IdemKey != "k19" {
		t.Fatalf("last record key = %s, want k19", recs[19].IdemKey)
	}
}

// 崩溃会在日志尾部留下残帧；重放必须截断它而不是丢弃整个日志。
func TestWALToleratesTornTail(t *testing.T) {
	path := t.TempDir() + "/t.wal"
	w, _, _ := OpenWAL(path)
	for i := 0; i < 5; i++ {
		w.Append(walRecord{Kind: evSubmitted, IdemKey: fmt.Sprintf("k%d", i),
			Want: Resources{CPU: 1}}, true)
	}
	w.Close()

	// 在尾部追加半条记录，模拟崩溃。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte{0x41, 0x4C, 0x53, 0x31, 0x00, 0x00})
	f.Close()

	w2, recs, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("reopen with torn tail: %v", err)
	}
	defer w2.Close()
	if len(recs) != 5 {
		t.Fatalf("replayed %d records, want 5 (torn tail ignored)", len(recs))
	}
	if w2.TruncatedTail() == 0 {
		t.Fatal("expected truncated tail bytes to be reported")
	}
}

// 每个 durable 追加都必须真正落盘，且 seq 不重复。
func TestWALConcurrentAppendAllDurable(t *testing.T) {
	path := t.TempDir() + "/c.wal"
	w, _, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	const n = 64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := w.Append(walRecord{Kind: evSubmitted,
				IdemKey: fmt.Sprintf("k%d", i), Want: Resources{CPU: 1}}, true); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	w.Close()

	_, recs, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n {
		t.Fatalf("durable records = %d, want %d", len(recs), n)
	}
	seen := make(map[string]bool)
	seqs := make(map[int64]bool)
	for _, r := range recs {
		if seen[r.IdemKey] {
			t.Fatalf("duplicate key %s", r.IdemKey)
		}
		if seqs[r.Seq] {
			t.Fatalf("duplicate seq %d", r.Seq)
		}
		seen[r.IdemKey] = true
		seqs[r.Seq] = true
	}
}

// ---------------------------------------------------------------------------
// keyedMutex（执行期同键互斥）
// ---------------------------------------------------------------------------

func TestKeyedMutexPreventsConcurrentSameKey(t *testing.T) {
	m := newKeyedMutex()
	release, ok := m.acquire("k")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	if _, ok := m.acquire("k"); ok {
		t.Fatal("second acquire of the same key must be refused")
	}
	if _, ok := m.acquire("other"); !ok {
		t.Fatal("different key should acquire fine")
	}
	release()
	if _, ok := m.acquire("k"); !ok {
		t.Fatal("acquire should succeed after release")
	}
}
