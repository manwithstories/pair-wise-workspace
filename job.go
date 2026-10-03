package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 资源三维
// ---------------------------------------------------------------------------

// Resources 描述一个作业对共享算力池的需求。三个维度都是单调递增的整数：
// CPU 单位为核，MemMB 单位为 MB，GPU 单位为卡。
type Resources struct {
	CPU   int64 `json:"cpu"`
	MemMB int64 `json:"mem_mb"`
	GPU   int64 `json:"gpu"`
}

func (r Resources) IsZero() bool { return r.CPU == 0 && r.MemMB == 0 && r.GPU == 0 }

func (r Resources) String() string {
	return fmt.Sprintf("cpu=%d mem=%dMB gpu=%d", r.CPU, r.MemMB, r.GPU)
}

func (r Resources) Validate() error {
	if r.CPU < 0 || r.MemMB < 0 || r.GPU < 0 {
		return fmt.Errorf("resources must be non-negative, got %s", r)
	}
	return nil
}

// Fits 判断 r 是否能整体塞进 available 剩余的三个维度里（要么全过，要么全不过）。
func (r Resources) Fits(available Resources) bool {
	return r.CPU <= available.CPU && r.MemMB <= available.MemMB && r.GPU <= available.GPU
}

// Cmp 返回三维字典序比较结果，用于把三维压成一个标量做稳定排序。
func (r Resources) Cmp(o Resources) int {
	if r.CPU != o.CPU {
		if r.CPU < o.CPU {
			return -1
		}
		return 1
	}
	if r.MemMB != o.MemMB {
		if r.MemMB < o.MemMB {
			return -1
		}
		return 1
	}
	if r.GPU != o.GPU {
		if r.GPU < o.GPU {
			return -1
		}
		return 1
	}
	return 0
}

// dominant 返回三维中的最大值，即 DRF 的「主导资源」维度。
func (r Resources) dominant() int64 {
	m := r.CPU
	if r.MemMB > m {
		m = r.MemMB
	}
	if r.GPU > m {
		m = r.GPU
	}
	return m
}

func addRes(a, b Resources) Resources {
	return Resources{CPU: a.CPU + b.CPU, MemMB: a.MemMB + b.MemMB, GPU: a.GPU + b.GPU}
}

func subRes(a, b Resources) Resources {
	return Resources{CPU: a.CPU - b.CPU, MemMB: a.MemMB - b.MemMB, GPU: a.GPU - b.GPU}
}

// ---------------------------------------------------------------------------
// 作业状态机
// ---------------------------------------------------------------------------

type JobState string

const (
	StatePending   JobState = "pending"
	StateRunning   JobState = "running"
	StateSucceeded JobState = "succeeded"
	StateFailed    JobState = "failed"
	StateCanceled  JobState = "canceled"
)

func (s JobState) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCanceled
}

var errBadTransition = errors.New("illegal state transition")

// DepRef 指向一个上游作业的幂等键。依赖只认幂等键，所以 WAL 重放后
// 上游换个内存地址也不影响依赖关系。
type DepRef struct {
	IdemKey string `json:"idem_key"`
}

func (d DepRef) String() string { return d.IdemKey }

// WorkSpec 描述作业该怎么跑。真实部署里由执行体（容器 / 训练脚本）接管，
// 这里保留一份可脚本化的规格，供内置 demo 制造失败与耗时。
type WorkSpec struct {
	Units        int           // 总计算单元数
	UnitDuration time.Duration // 每个单元耗时
	FailFirst    int           // 前 N 次尝试返回可重试错误（0 表示永不失败）
	FailAlways   bool          // 每次尝试都失败（用于把熔断器打到半开放）
}

// Dependencies 允许按名字引用上游作业，避免 CLI 里手写幂等键。
type Dependencies []string

// ---------------------------------------------------------------------------
// Job
// ---------------------------------------------------------------------------

// Job 是调度器的最小工作单元。它携带幂等键（既是幂等提交的去重依据，
// 也是执行期的互斥键）、重试计数以及 DRF 打分需要的全部输入。
//
// Job 会被调度主循环、errgroup 执行体和 WAL 回写三处并发触碰，
// 因此可变字段一律走 mu 保护，不允许外部裸读。
type Job struct {
	// ---- 只读字段，构造后不再修改 ----
	Seq         int64     // 全局单调序号，FIFO 兜底排序用
	Tenant      string    // 租户，DRF 份额按它聚合
	ReleaseHold string    // 供资源释放使用的租户副本（见 newJob 注释）
	IdemKey     string    // 幂等键
	Group       string    // 作业组 ID，同组共享一个 errgroup 上下文
	Name        string    // 人类可读名字，仅用于日志
	Want        Resources // 资源请求
	DepRefs     []DepRef  // 上游作业的幂等键
	MaxRetries  int       // 最多额外重试次数（含首次共 MaxRetries+1 次尝试）
	Work        WorkSpec  // 可脚本化的执行规格
	Recovered   bool      // 由 WAL 重放恢复而来
	released    bool      // 资源是否已归还（幂等保护，防止重复 release）

	// ---- 受 mu 保护的字段 ----
	mu           sync.Mutex
	state        JobState
	attempts     int
	retryCount   int
	lastErr      error
	submittedAt  time.Time
	startedAt    time.Time
	finishedAt   time.Time
	waitStart    time.Time // 进入 ready 队列的时刻，用于饥饿老化补偿
	cancelled    bool      // 上游失败 / 优雅停机强制取消
	cancelWhy    string
	accounted    bool  // 调度器已完成租户账本记账（幂等保护）
	planned      bool  // 已被某轮 DRF 决策选中，正在扣资源
	groupAttempt int64 // 最近一次加入作业组时的尝试序号（用于重试去重）

	// ---- 仅调度器主循环访问，无须加锁 ----
	score    float64 // 最近一次 DRF 打分（含老化补偿）
	scoreSeq int64   // 打分代数，堆内做过期项检测
}

func newJob(seq int64, tenant, idemKey string, want Resources, deps []DepRef, work WorkSpec, maxRetries int) *Job {
	if maxRetries < 0 {
		maxRetries = 0
	}
	return &Job{
		Seq:         seq,
		Tenant:      tenant,
		ReleaseHold: tenant,
		IdemKey:     idemKey,
		Want:        want,
		DepRefs:     deps,
		MaxRetries:  maxRetries,
		Work:        work,
		state:       StatePending,
		submittedAt: time.Now(),
		waitStart:   time.Now(),
	}
}

func (j *Job) State() JobState {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

func (j *Job) Attempts() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.attempts
}

func (j *Job) Retries() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.retryCount
}

func (j *Job) LastError() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastErr
}

func (j *Job) String() string {
	return fmt.Sprintf("job[%s tenant=%s name=%s want=%s]", j.IdemKey, j.Tenant, j.Name, j.Want)
}

// transition 推进状态机，并顺带维护时间戳。非法跃迁直接 panic——调度器
// 内部逻辑出错时宁可炸掉，也不要让资源账本和状态机悄悄对不上。
func (j *Job) transition(to JobState, reason string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	from := j.state
	if !validTransition(from, to) {
		return fmt.Errorf("%w: %s %s->%s (%s)", errBadTransition, j.IdemKey, from, to, reason)
	}
	switch to {
	case StateRunning:
		j.startedAt = time.Now()
		j.attempts++
		// 新一次尝试开始，清掉上一轮的失败原因。
		//
		// 不清的话，第三次尝试成功之后 lastErr 里仍然留着第二次的
		// "scripted transient failure"，settleJob 会据此判定「又失败了」，
		// 于是成功的那次尝试被当成又一次失败——重试永远收敛不了，
		// 作业也永远不会被判定为终态、一直挂在活跃索引里。
		j.lastErr = nil
	case StatePending:
		// 退回待调度：清掉上一轮的起止时间，Duration 才有意义。
		j.finishedAt = time.Time{}
	case StateSucceeded, StateFailed, StateCanceled:
		j.finishedAt = time.Now()
		if j.startedAt.IsZero() {
			j.startedAt = time.Now()
		}
	}
	j.state = to
	if to == StateRunning {
		j.cancelled = false
	}
	return nil
}

func validTransition(from, to JobState) bool {
	switch from {
	case StatePending:
		return to == StateRunning || to == StateCanceled || to == StateFailed
	case StateRunning:
		// running -> pending 是退避重试的正常跃迁：执行体已经返回并释放了资源，
		// 作业重新回到可调度状态。
		return to == StateSucceeded || to == StateFailed || to == StateCanceled ||
			to == StateRunning || to == StatePending
	default:
		return false
	}
}

// markReady 把作业从「依赖未满足」推进到「可调度」，重置老化计时。
func (j *Job) markReady() {
	j.mu.Lock()
	j.waitStart = time.Now()
	j.mu.Unlock()
}

// waitFor 作业在 ready 队列里已经等了多久，用于 DRF 的饥饿老化补偿。
func (j *Job) waitFor(now time.Time) time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.waitStart.IsZero() {
		return 0
	}
	return now.Sub(j.waitStart)
}

func (j *Job) setLastError(err error) {
	j.mu.Lock()
	j.lastErr = err
	j.mu.Unlock()
}

func (j *Job) setRetryCount(n int) {
	j.mu.Lock()
	j.retryCount = n
	j.mu.Unlock()
}

// markCanceled 标记作业被上游失败或优雅停机超时强制取消。
func (j *Job) markCanceled(why string) {
	j.mu.Lock()
	j.cancelled = true
	j.cancelWhy = why
	j.mu.Unlock()
}

func (j *Job) isCanceled() (bool, string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelled, j.cancelWhy
}

func (j *Job) Duration() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finishedAt.IsZero() || j.startedAt.IsZero() {
		return 0
	}
	return j.finishedAt.Sub(j.startedAt)
}

// ---------------------------------------------------------------------------
// 提交请求
// ---------------------------------------------------------------------------

// SubmitRequest 是一次提交的完整描述。
type SubmitRequest struct {
	Tenant     string
	IdemKey    string
	Group      string
	Name       string
	Want       Resources
	Deps       Dependencies
	DepRefs    []DepRef // 显式依赖引用（CLI 用 Deps，内部用 DepRefs）
	MaxRetries int
	Work       WorkSpec
	// Force 绕过「同键只入队一次」的闸门。恢复流程和运维重投需要它，
	// 但真正的防双跑保证在执行期的 keyedMutex 上，不在这里。
	Force bool
}

func (r SubmitRequest) Validate() error {
	if strings.TrimSpace(r.Tenant) == "" {
		return errors.New("tenant is required")
	}
	if strings.TrimSpace(r.IdemKey) == "" {
		return errors.New("idem key is required")
	}
	if err := r.Want.Validate(); err != nil {
		return err
	}
	if r.MaxRetries < 0 {
		return errors.New("max-retries must be >= 0")
	}
	return nil
}

// depRefs 返回作业的依赖列表（只读，构造后不变）。
func (j *Job) depRefs() []DepRef { return j.DepRefs }

// ---------------------------------------------------------------------------
// 决策预约
// ---------------------------------------------------------------------------

// beginPlan 抢占「调度决策」权。只有拿到它的作业才允许进入 Acquire，
// 防止并发 Plan 选中同一个作业。
func (j *Job) beginPlan() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.planned {
		return false
	}
	j.planned = true
	return true
}

// abortPlan 撤销决策预约（Acquire 失败时调用）。
func (j *Job) abortPlan() {
	j.mu.Lock()
	j.planned = false
	j.mu.Unlock()
}

// markAccounted 标记调度器已完成该作业的租户账本记账。
func (j *Job) markAccounted() {
	j.mu.Lock()
	j.accounted = true
	j.mu.Unlock()
}

// Accounted 返回该作业是否已被调度器结算过。
func (j *Job) Accounted() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.accounted
}

// inGroupAttempt 判断本作业是否已经以该尝试序号加入过组。
func (j *Job) inGroupAttempt(seq int64) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.groupAttempt == seq
}

// setGroupAttempt 记录本次加入作业组使用的尝试序号。
func (j *Job) setGroupAttempt(seq int64) {
	j.mu.Lock()
	j.groupAttempt = seq
	j.mu.Unlock()
}
