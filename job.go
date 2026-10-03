// job.go — 作业状态机、幂等键、重试计数与资源请求描述。
//
// 状态机主线（题面要求 pending→running→succeeded/failed）：
//
//	pending ──▶ running ──▶ succeeded
//	   │           │
//	   │           ├──────▶ failed     (尝试耗尽 / 不可重试错误)
//	   │           ├──────▶ canceled   (优雅停机或上游失败传播)
//	   │           └──────▶ pending    (指数退避重试，Attempt+1)
//	   ├──────▶ rejected                 (熔断 / 幂等命中 / 请求超池)
//	   └──────▶ canceled
//
// 终态为 succeeded / failed / rejected / canceled；非终态可继续流转。
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// JobState 是作业生命周期状态。
type JobState string

const (
	StatePending   JobState = "pending"
	StateRunning   JobState = "running"
	StateSucceeded JobState = "succeeded"
	StateFailed    JobState = "failed"
	StateCanceled  JobState = "canceled"
	StateRejected  JobState = "rejected"
)

// allowedTransitions 是状态机的唯一事实来源；任何未列出的流转都会被拒绝，
// 从而保证 WAL 回写与内存状态不会互相撕裂。
var allowedTransitions = map[JobState]map[JobState]bool{
	StatePending: {
		StateRunning: true, StateCanceled: true, StateRejected: true, StateFailed: true,
	},
	StateRunning: {
		StateSucceeded: true, StateFailed: true, StateCanceled: true, StatePending: true,
	},
	StateSucceeded: {},
	StateFailed:    {},
	StateCanceled:  {},
	StateRejected:  {},
}

// Terminal 表示该状态是否不可再离开。
func (s JobState) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCanceled, StateRejected:
		return true
	}
	return false
}

// ErrBadTransition 在状态机被非法穿越时返回。
var ErrBadTransition = errors.New("非法状态流转")

// Job 是一次批处理作业的完整描述。
//
// Run 是纯内存的执行体（不可序列化）；跨重启恢复时由 main.go 的 kind 注册表
// 按 Job.Kind 重新水合（rehydrate）。这是 WAL 只记录状态、不记录闭包的结果。
type Job struct {
	ID          string
	Tenant      string
	Kind        string // 逻辑任务类型，跨重启水合执行的依据
	Group       string // 依赖组 ID；同组作业共享一个 errgroup
	IdemKey     string // 幂等键：同键只入队一次，执行期加互斥锁
	Req         Resources
	State       JobState
	Attempt     int // 已尝试次数（含本次）
	MaxAttempts int

	Seq        uint64 // 全局提交序号，用于 FIFO 与稳定排序
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	NotBefore  time.Time // 指数退避：早于该时刻不参与调度
	DependsOn  []string  // 上游作业 ID

	LastError string
	Recovered bool // 由 WAL 重放恢复出来

	// Run 为 nil 时使用注册表按 Kind 水合（见 main.go 的 taskRegistry）。
	Run func(ctx context.Context, j *Job) error

	mu       sync.Mutex
	done     chan struct{}
	doneOnce sync.Once
	ready    bool // 是否当前在 DRF 优先队列中（防重复入堆）
}

// JobSpec 是提交接口的入参。
type JobSpec struct {
	ID          string
	Tenant      string
	Kind        string
	Group       string // 依赖组 ID；同组作业共享一个 errgroup
	IdemKey     string
	Req         Resources
	DependsOn   []string
	MaxAttempts int
	Run         func(ctx context.Context, j *Job) error
}

// NewJob 规整并校验一份提交请求。
func NewJob(spec JobSpec, seq uint64, now time.Time) (*Job, error) {
	if spec.Tenant == "" {
		return nil, errors.New("租户不能为空")
	}
	if err := spec.Req.Validate(); err != nil {
		return nil, fmt.Errorf("作业 %q 资源请求非法: %w", spec.ID, err)
	}
	if spec.MaxAttempts <= 0 {
		spec.MaxAttempts = DefaultMaxAttempts
	}
	j := &Job{
		ID:          spec.ID,
		Tenant:      spec.Tenant,
		Kind:        spec.Kind,
		Group:       spec.Group,
		IdemKey:     spec.IdemKey,
		Req:         spec.Req,
		Attempt:     0,
		MaxAttempts: spec.MaxAttempts,
		Seq:         seq,
		CreatedAt:   now,
		NotBefore:   now,
		State:       StatePending,
		DependsOn:   append([]string(nil), spec.DependsOn...),
		Run:         spec.Run,
		done:        make(chan struct{}),
	}
	if j.ID == "" {
		return nil, errors.New("作业 ID 不能为空")
	}
	if j.IdemKey == "" {
		j.IdemKey = j.ID // 未显式给幂等键时退化为按 ID 去重
	}
	if j.Kind == "" {
		j.Kind = KindNoop
	}
	return j, nil
}

// transition 在持有作业锁的前提下做状态机校验并写入新状态。
func (j *Job) transition(to JobState) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if allowedTransitions[j.State][to] {
		j.State = to
		return nil
	}
	return fmt.Errorf("%w: %s %s -> %s", ErrBadTransition, j.ID, j.State, to)
}

// markUnconditional 用于终态回写（WAL 已是事实，不允许被状态机拒绝）。
func (j *Job) forceState(to JobState) {
	j.mu.Lock()
	j.State = to
	j.mu.Unlock()
}

// StateOf 读取当前状态。
func (j *Job) StateOf() JobState {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.State
}

// setNotBefore / setError / setAttempt 等字段级更新，都在作业锁下完成。
func (j *Job) setError(err error) {
	j.mu.Lock()
	if err != nil {
		j.LastError = err.Error()
	}
	j.mu.Unlock()
}

func (j *Job) setNotBefore(t time.Time) {
	j.mu.Lock()
	j.NotBefore = t
	j.mu.Unlock()
}

func (j *Job) attemptOf() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Attempt
}

// beginAttempt 原子地把尝试计数 +1；返回 false 表示重试次数已耗尽。
func (j *Job) beginAttempt() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Attempt >= j.MaxAttempts {
		return false
	}
	j.Attempt++
	if j.StartedAt.IsZero() {
		j.StartedAt = time.Now()
	}
	return true
}

// signalDone 唤醒所有等待该作业终态的下游作业（每作业仅生效一次）。
func (j *Job) signalDone() {
	j.doneOnce.Do(func() { close(j.done) })
}

// DoneChan 是下游作业等待上游完成的通道。
func (j *Job) DoneChan() <-chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done == nil {
		j.done = make(chan struct{})
	}
	return j.done
}

// canRetry 判断失败后是否还能按指数退避重试。
func (j *Job) canRetry(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false // 取消类错误重试无意义
	}
	return j.attemptOf() < j.MaxAttempts
}

// RetryBackoff 计算第 attempt 次失败后的退避时长：base * 2^(attempt-1)，并加
// 确定性抖动（基于作业序号），避免同租户多个失败作业同时重试形成尖峰。
func RetryBackoff(base time.Duration, attempt int, seq uint64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 6 {
		shift = 6
	}
	d := base << uint(shift)
	jitter := time.Duration(float64(d) * (0.85 + 0.3*float64((seq*2654435761)%100)/100.0))
	return jitter
}

// JobView 是对外只读快照，避免终端与 WAL 直接读可变作业对象。
type JobView struct {
	ID          string    `json:"id"`
	Tenant      string    `json:"tenant"`
	Kind        string    `json:"kind"`
	Group       string    `json:"group,omitempty"`
	IdemKey     string    `json:"idem_key"`
	State       JobState  `json:"state"`
	Req         Resources `json:"req"`
	Attempt     int       `json:"attempt"`
	MaxAttempts int       `json:"max_attempts"`
	LastError   string    `json:"last_error,omitempty"`
	Recovered   bool      `json:"recovered,omitempty"`
}

// View 生成作业只读快照。
func (j *Job) View() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return JobView{
		ID: j.ID, Tenant: j.Tenant, Kind: j.Kind, Group: j.Group, IdemKey: j.IdemKey,
		State: j.State, Req: j.Req, Attempt: j.Attempt, MaxAttempts: j.MaxAttempts,
		LastError: j.LastError, Recovered: j.Recovered,
	}
}
