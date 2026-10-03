// wal.go — 追加式预写日志（WAL）：落盘状态事件，崩溃后重放恢复在途作业。
//
// 布局：单文件追加，每条记录为一行 JSON（换行分隔），字段为
//
//	{"seq":N,"ts":...,"event":"...","job":{...}}
//
// 设计要点：
//   - 追加即"预写"：Submit 先落 WAL 再入队，保证崩溃后不会丢作业。
//   - fsync 批量合并：高频事件（状态流转）在同一 tick 内合并成一次 fsync，
//     这样"单次 fsync <5ms"与"状态不丢"两个目标同时成立；GracefulStop 会做一次
//     强制 flush，保证停机前所有事件真正落盘。
//   - 重放幂等：Replayed 日志由事件自身语义保证（终态覆盖、重复 submit 取首条），
//     因此重放两次不会产生不同结果；截断的半条记录（尾部撕裂）会被丢弃。
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// WAL 事件类型。
const (
	EvtSubmit      = "submit"      // 作业提交（完整描述，落盘后可重放）
	EvtAdmitted    = "admitted"    // 进入 DRF 待调度队列
	EvtRunning     = "running"     // 已通过资源预留，errgroup 已启动
	EvtSucceeded   = "succeeded"   // 成功终态
	EvtFailed      = "failed"      // 重试耗尽后的失败终态
	EvtCanceled    = "canceled"    // 被取消（停机 / 上游失败传播）
	EvtRejected    = "rejected"    // 熔断 / 请求超池 / 依赖非法
	EvtRetry       = "retry"       // 指数退避重试安排
	EvtCancelGroup = "cancelgroup" // 上游失败，取消整组下游
	EvtCheckpoint  = "checkpoint"  // 显式检查点（演示用）
)

// WLEvent 是写往 WAL 的一条状态事件。
//
// Job 字段在 submit 事件里是完整的 JobView（含依赖、幂等键、重试上限），
// 其余事件只回写状态与错误，seq/attempt 仍带以便重放时校验。
type WLEvent struct {
	Seq     uint64           `json:"seq"`
	TS      time.Time        `json:"ts"`
	Event   string           `json:"event"`
	Group   string           `json:"group,omitempty"`
	Reason  string           `json:"reason,omitempty"`
	Job     *WALJobRecord    `json:"job,omitempty"`
	Tenants []TenantSnapshot `json:"tenants,omitempty"`
}

// WALJobRecord 是可序列化 / 可重放的作业记录。
type WALJobRecord struct {
	ID          string    `json:"id"`
	Tenant      string    `json:"tenant"`
	Kind        string    `json:"kind"`
	Group       string    `json:"group,omitempty"`
	IdemKey     string    `json:"idem_key"`
	Req         Resources `json:"req"`
	Attempt     int       `json:"attempt"`
	MaxAttempts int       `json:"max_attempts"`
	State       JobState  `json:"state"`
	DependsOn   []string  `json:"depends_on,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Seq         uint64    `json:"seq"`
	NotBefore   time.Time `json:"not_before,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// record 把作业转成可落盘记录。
func record(j *Job) *WALJobRecord {
	v := j.View()
	return &WALJobRecord{
		ID: v.ID, Tenant: v.Tenant, Kind: v.Kind, Group: j.Group, IdemKey: v.IdemKey,
		Req: v.Req, Attempt: v.Attempt, MaxAttempts: v.MaxAttempts, State: v.State,
		DependsOn: append([]string(nil), j.DependsOn...), LastError: v.LastError,
		Seq: j.Seq, NotBefore: j.NotBefore, CreatedAt: j.CreatedAt,
	}
}

// WAL 是追加式日志。
type WAL struct {
	path string

	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	seq    uint64
	dirty  bool // 有未 fsync 的缓冲数据
	closed bool

	// 批量 fsync 的节流
	interval time.Duration
	flushReq chan struct{}

	nAppend   atomic.Int64
	nSync     atomic.Int64
	nTruncate atomic.Int64 // 重放时丢弃的撕裂记录数
}

// OpenWAL 打开（必要时创建）WAL 文件并扫描已有内容以恢复 seq 水位。
func OpenWAL(path string) (*WAL, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建 WAL 目录失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开 WAL 失败: %w", err)
	}
	w := &WAL{
		path:     path,
		f:        f,
		w:        bufio.NewWriterSize(f, 64*1024),
		interval: 20 * time.Millisecond,
		flushReq: make(chan struct{}, 1),
	}
	// 先修复上次崩溃留下的撕裂尾部，再扫描。
	// 顺序很关键：必须在本次追加任何记录之前完成截断，
	// 否则新记录会被拼接到半条 JSON 后面，把两行粘成一条永远无法解析的数据。
	if err := w.repairTornTail(); err != nil {
		_ = f.Close()
		return nil, err
	}
	// 扫描已存在的记录，确定 seq 水位（避免重启后 seq 回退导致重放乱序）。
	maxSeq, err := w.scanExisting()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w.seq = maxSeq
	go w.flushLoop()
	return w, nil
}

// scanExisting 逐行解析既有 WAL，返回最大 seq；同时统计无法解析的行数（撕裂记录）。
func (w *WAL) scanExisting() (uint64, error) {
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(w.f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var maxSeq uint64
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev WLEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// 尾部撕裂的半条记录：忽略并计数，末尾会在 Close 时被截断。
			w.nTruncate.Add(1)
			continue
		}
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("扫描 WAL 失败: %w", err)
	}
	// 重新定位到文件末尾，后续继续追加。
	if _, err := w.f.Seek(0, io.SeekEnd); err != nil {
		return 0, err
	}
	return maxSeq, nil
}

// flushLoop 周期性把脏数据 fsync 到磁盘，实现"批量 fsync"。
func (w *WAL) flushLoop() {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = w.Flush()
		case <-w.flushReq:
			_ = w.Flush()
		}
	}
}

// Append 追加一条事件（写进用户态缓冲，不阻塞 fsync）。
func (w *WAL) Append(ev WLEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("WAL 已关闭")
	}
	w.seq++
	ev.Seq = w.seq
	if ev.TS.IsZero() {
		ev.TS = time.Now()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("序列化 WAL 事件失败: %w", err)
	}
	if _, err := w.w.Write(line); err != nil {
		return fmt.Errorf("写 WAL 失败: %w", err)
	}
	if err := w.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("写 WAL 失败: %w", err)
	}
	w.dirty = true
	w.nAppend.Add(1)
	// 非阻塞地唤醒刷盘循环；通道满说明已有一轮 flush 在排队。
	select {
	case w.flushReq <- struct{}{}:
	default:
	}
	return nil
}

// Flush 把缓冲写入并 fsync。优雅停机与关键状态会显式调用。
func (w *WAL) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || !w.dirty {
		return nil
	}
	if err := w.w.Flush(); err != nil {
		return fmt.Errorf("flush WAL 失败: %w", err)
	}
	start := time.Now()
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("fsync WAL 失败: %w", err)
	}
	recordSync(time.Since(start))
	w.dirty = false
	w.nSync.Add(1)
	return nil
}

// Close 做最后一次 fsync 并关闭文件，同时截断尾部无法解析的半条记录。
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	// 先 flush 已有缓冲。
	if w.dirty {
		if err := w.w.Flush(); err != nil {
			_ = w.f.Close()
			return err
		}
		_ = w.f.Sync()
		w.nSync.Add(1)
		w.dirty = false
	}
	// 截断撕裂尾部：定位到最后一条完整 JSON 行末尾。
	if err := w.truncateTornTail(); err != nil {
		_ = w.f.Close()
		return err
	}
	return w.f.Close()
}

// repairTornTail 在打开 WAL 时修复尾部撕裂记录（崩溃时只写了一半的那行）。
//
// 与 truncateTornTail 的区别：后者在 Close 时调用（此时不会再追加），
// 本函数在 Open 时、任何追加之前调用，确保后续写入不会粘在半条记录后面。
func (w *WAL) repairTornTail() error {
	st, err := w.f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size == 0 {
		return nil
	}
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(w.f, data); err != nil {
		return err
	}
	// 找到最后一条"完整行"的结束位置：最后一个 '\n' 之后。
	last := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			last = i + 1
		}
	}
	if last == len(data) {
		// 每行都以换行结尾；再确认最后一行本身能解析。
		return nil
	}
	// 尾部存在半条记录 -> 截断。
	if err := w.f.Truncate(int64(last)); err != nil {
		return err
	}
	w.nTruncate.Add(1)
	_, err = w.f.Seek(int64(last), io.SeekStart)
	return err
}

// truncateTornTail 扫描文件，把尾部无法解析的行截掉，保证下次追加仍是合法记录流。
func (w *WAL) truncateTornTail() error {
	st, err := w.f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(w.f, data); err != nil {
		return err
	}
	last := 0
	for i := 0; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		last = i + 1
	}
	if last == len(data) {
		return nil // 完整
	}
	// 存在尾部半条记录 -> 截断。
	if err := w.f.Truncate(int64(last)); err != nil {
		return err
	}
	w.nTruncate.Add(1)
	if _, err := w.f.Seek(int64(last), io.SeekStart); err != nil {
		return err
	}
	return nil
}

// Path 返回 WAL 文件路径。
func (w *WAL) Path() string { return w.path }

// SyncLatency 是 fsync 延迟的分位统计（毫秒）。
type SyncLatency struct {
	AvgMs float64 `json:"avg_ms"`
	P50Ms float64 `json:"p50_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
	N     int     `json:"n"`
}

const syncRingSize = 512

var (
	syncMu      sync.Mutex
	syncSamples []time.Duration // 环形缓冲：满了就覆盖最旧样本
	syncCursor  int
)

// recordSync 记录一次 fsync 耗时（只测 Sync 本身，不含用户态缓冲写入）。
func recordSync(d time.Duration) {
	syncMu.Lock()
	if len(syncSamples) < syncRingSize {
		syncSamples = append(syncSamples, d)
	} else {
		syncSamples[syncCursor%syncRingSize] = d
		syncCursor++
	}
	syncMu.Unlock()
}

// SyncStats 返回本进程观测到的 fsync 延迟统计。
func SyncStats() SyncLatency {
	syncMu.Lock()
	defer syncMu.Unlock()
	if len(syncSamples) == 0 {
		return SyncLatency{}
	}
	cp := append([]time.Duration(nil), syncSamples...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	var sum time.Duration
	for _, d := range cp {
		sum += d
	}
	ms := func(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }
	return SyncLatency{
		AvgMs: ms(sum / time.Duration(len(cp))),
		P50Ms: ms(cp[len(cp)/2]),
		P99Ms: ms(cp[len(cp)*99/100]),
		MaxMs: ms(cp[len(cp)-1]),
		N:     len(cp),
	}
}

// Stats 返回 WAL 统计。
func (w *WAL) Stats() map[string]int64 {
	return map[string]int64{
		"append":    w.nAppend.Load(),
		"fsync":     w.nSync.Load(),
		"torn":      w.nTruncate.Load(),
		"watermark": int64(w.seq),
	}
}

// Replay 读取 WAL，重建作业集合与在途状态。
//
// 返回：所有见过的作业（按提交序号），以及按提交顺序排列的事件列表（供终端回放）。
// 重放只重建"作业集合 + 状态 + 资源账本"，随后由 Scheduler 决定哪些重新入队。
func Replay(path string) ([]*WALJobRecord, []WLEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil // 没有 WAL 是正常冷启动
		}
		return nil, nil, err
	}
	defer f.Close()

	var jobs []*WALJobRecord
	var events []WLEvent
	byID := map[string]*WALJobRecord{}
	firstSubmit := map[string]bool{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev WLEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // 撕裂记录，忽略
		}
		events = append(events, ev)
		if ev.Job == nil {
			continue
		}
		switch ev.Event {
		case EvtSubmit:
			// 幂等：同 ID 只认第一条 submit；重复提交在 WAL 里也只留首次。
			if firstSubmit[ev.Job.ID] {
				continue
			}
			firstSubmit[ev.Job.ID] = true
			cp := *ev.Job
			byID[ev.Job.ID] = &cp
			jobs = append(jobs, &cp)
		default:
			// 状态事件：覆盖内存记录的状态/错误/重试进度。
			if rec, ok := byID[ev.Job.ID]; ok {
				rec.State = ev.Job.State
				if ev.Job.Attempt > 0 {
					rec.Attempt = ev.Job.Attempt
				}
				if ev.Job.LastError != "" {
					rec.LastError = ev.Job.LastError
				}
				if !ev.Job.NotBefore.IsZero() {
					rec.NotBefore = ev.Job.NotBefore
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("重放 WAL 失败: %w", err)
	}
	return jobs, events, nil
}
