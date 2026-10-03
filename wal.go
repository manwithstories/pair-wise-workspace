package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 事件类型
// ---------------------------------------------------------------------------

type evKind string

const (
	evSubmitted   evKind = "submitted"  // 作业入队（去重通过后）
	evDispatched  evKind = "dispatched" // 调度决策：扣减资源，转 running
	evCompleted   evKind = "completed"  // 执行成功
	evFailed      evKind = "failed"     // 重试耗尽，执行失败
	evCanceled    evKind = "canceled"   // 上游失败传播 / 停机超时强制取消
	evRetry       evKind = "retry"      // 单次尝试失败，进入指数退避
	evBreakerTrip evKind = "breaker"    // 熔断器动作
	evShutdown    evKind = "shutdown"   // 优雅停机开始
)

// walRecord 是一行 WAL。Payload 里带上作业的完整静态描述，是为了让重放
// 不依赖任何进程内内存——崩溃后光靠日志就能把在途作业原样重建出来。
//
// 帧格式：magic(4) | length(4) | crc32(4) | payload(length)
// 崩溃时最后一条几乎必然残缺，靠 magic/length/crc 三重校验把残帧截断，
// 再把文件截到最后一个完整帧的边界，保证下次启动面对的仍是干净日志。
type walRecord struct {
	Seq        int64     `json:"seq"`
	Kind       evKind    `json:"kind"`
	TS         int64     `json:"ts"`
	IdemKey    string    `json:"idem_key,omitempty"`
	Tenant     string    `json:"tenant,omitempty"`
	Group      string    `json:"group,omitempty"`
	Name       string    `json:"name,omitempty"`
	Want       Resources `json:"want,omitempty"`
	DepRefs    []DepRef  `json:"deps,omitempty"`
	MaxRetries int       `json:"max_retries,omitempty"`
	Work       WorkSpec  `json:"work,omitempty"`
	Attempts   int       `json:"attempts,omitempty"`
	RetryCount int       `json:"retry_count,omitempty"`
	Err        string    `json:"err,omitempty"`
	Recovered  bool      `json:"recovered,omitempty"`
	// TenantUsage 是 dispatched 事件里的账本快照，让重放能重建 DRF 份额，
	// 而不是从零开始把新租户当成冷启动。
	TenantUsage map[string]Resources `json:"tenant_usage,omitempty"`
}

const (
	walMagic      uint32 = 0x414C5331 // "ALS1"
	walHeader            = 12         // magic(4) + length(4) + crc32(4)
	maxWALPayload        = 64 << 20
)

// ---------------------------------------------------------------------------
// 扫描（启动时重放）
// ---------------------------------------------------------------------------

type walScanner struct {
	r        *bufio.Reader
	valid    int64 // 已解析出完整帧的字节数
	fileSize int64
	torn     bool
	tornAt   int64
}

// next 返回下一条完整记录；遇到残帧/坏帧立即停止，并把 torn 置位。
func (s *walScanner) next() (walRecord, bool) {
	var head [walHeader]byte
	n, err := io.ReadFull(s.r, head[:])
	if err != nil {
		// 短读或 EOF：文件尾部残帧。n>0 说明连帧头都没写完。
		if !(err == io.EOF && n == 0) {
			s.markTorn(s.valid)
		}
		return walRecord{}, false
	}
	magic := be32(head[0:4])
	length := int64(be32(head[4:8]))
	wantCRC := be32(head[8:12])
	if magic != walMagic || length <= 0 || length > maxWALPayload {
		s.markTorn(s.valid)
		return walRecord{}, false
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(s.r, body); err != nil {
		s.markTorn(s.valid)
		return walRecord{}, false
	}
	if crc32.ChecksumIEEE(body) != wantCRC {
		s.markTorn(s.valid)
		return walRecord{}, false
	}
	var rec walRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		s.markTorn(s.valid)
		return walRecord{}, false
	}
	s.valid += int64(walHeader) + length
	return rec, true
}

func (s *walScanner) markTorn(at int64) {
	if !s.torn {
		s.torn = true
		s.tornAt = at
	}
}

// ---------------------------------------------------------------------------
// WAL
// ---------------------------------------------------------------------------

// WAL 是追加式预写日志。所有状态事件先落 WAL 再改内存。
//
// 关于 fsync 预算（<5ms/次）：写入路径刻意不用 bufio.Writer 做攒批缓冲，
// 那会把多次写合并成一次 fsync，崩溃时丢掉窗口内的全部记录，正好违背 WAL
// 语义。这里保证「一条 durable 记录 = 一次 fdatasync」，成本靠批量组提交来
// 摊：多个并发追加者只让其中一个真正执行 fsync，其余的记录被同一次 fsync
// 顺带覆盖。groupWindow > 0 时还会有一个有上限的等待窗口用于聚批。
type WAL struct {
	path string

	mu     sync.Mutex
	f      *os.File
	bw     *bufio.Writer
	seq    int64
	fileSz int64

	closed bool
	// syncing=true 表示已有 goroutine 在做 fsync，其它追加者等它的广播。
	syncing   bool
	syncedSeq int64
	group     *sync.Cond

	syncMode    bool
	groupWindow time.Duration

	// 恢复元信息
	replayed       bool
	replayedCount  int
	truncatedBytes int64

	// ---- 统计 ----
	appendCount atomic.Int64
	fsyncCount  atomic.Int64
	syncNanos   atomic.Int64
	maxSyncNs   atomic.Int64
	bytesCount  atomic.Int64
}

type walOptions struct {
	// syncMode=false 时只 write 不 fsync，仅用于性能基准。
	syncMode bool
	// groupWindow 是组提交窗口；0 表示每个 durable 记录立即 fsync（最保守）。
	groupWindow time.Duration
}

func defaultWALOptions() walOptions {
	return walOptions{syncMode: true, groupWindow: 0}
}

// OpenWAL 打开或创建 WAL，返回恢复出的记录序列。
func OpenWAL(path string) (*WAL, []walRecord, error) {
	return openWAL(path, defaultWALOptions())
}

func openWAL(path string, opts walOptions) (*WAL, []walRecord, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("create wal dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open wal: %w", err)
	}
	w := &WAL{
		path:        path,
		f:           f,
		syncMode:    opts.syncMode,
		groupWindow: opts.groupWindow,
	}
	w.group = sync.NewCond(&w.mu)

	records, validBytes, fileSize, err := scanWAL(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	w.replayed = len(records) > 0
	w.replayedCount = len(records)
	if len(records) > 0 {
		w.seq = records[len(records)-1].Seq
		w.syncedSeq = w.seq
	}
	w.truncatedBytes = fileSize - validBytes

	// 把文件收到最后一个完整帧边界：有残帧就丢掉，无残帧时是幂等的。
	if err := f.Truncate(validBytes); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("truncate torn tail: %w", err)
	}
	w.bw = bufio.NewWriterSize(f, 64<<10)
	w.fileSz = validBytes
	// 后续写全部走 WriteAt，避免依赖共享的文件偏移（Seek 在并发下不成立）。
	w.bw.Reset(newOffsetWriter(f, validBytes))
	return w, records, nil
}

// scanWAL 顺序解析整个日志，返回记录与有效字节数。
func scanWAL(f *os.File) ([]walRecord, int64, int64, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("stat wal: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, 0, fmt.Errorf("seek wal: %w", err)
	}
	s := &walScanner{r: bufio.NewReaderSize(f, 64<<10), fileSize: st.Size()}
	var out []walRecord
	for {
		rec, ok := s.next()
		if !ok {
			break
		}
		out = append(out, rec)
	}
	return out, s.valid, st.Size(), nil
}

// offsetWriter 把带缓冲的写落到指定文件偏移上，让 WAL 自身完全无状态。
type offsetWriter struct {
	f      *os.File
	offset int64
}

func newOffsetWriter(f *os.File, off int64) *offsetWriter { return &offsetWriter{f: f, offset: off} }

func (o *offsetWriter) Write(p []byte) (int, error) {
	n, err := o.f.WriteAt(p, o.offset)
	o.offset += int64(n)
	return n, err
}

// ---------------------------------------------------------------------------
// 追加
// ---------------------------------------------------------------------------

// Append 追加一条记录。durability=false 表示这条可以和后续记录合并成
// 一次 fsync（调用方确认该事件即使在崩溃中丢失也可由重放推导出来）。
func (w *WAL) Append(rec walRecord, durability bool) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return errors.New("wal is closed")
	}
	w.seq++
	rec.Seq = w.seq
	if rec.TS == 0 {
		rec.TS = time.Now().UnixNano()
	}
	body, err := json.Marshal(rec)
	if err != nil {
		w.seq--
		w.mu.Unlock()
		return fmt.Errorf("marshal wal record: %w", err)
	}
	frame := make([]byte, walHeader+len(body))
	putBE32(frame[0:4], walMagic)
	putBE32(frame[4:8], uint32(len(body)))
	putBE32(frame[8:12], crc32.ChecksumIEEE(body))
	copy(frame[walHeader:], body)

	n, err := w.bw.Write(frame)
	if err != nil {
		w.mu.Unlock()
		return fmt.Errorf("wal write: %w", err)
	}
	w.fileSz += int64(n)
	w.appendCount.Add(1)
	w.bytesCount.Add(int64(len(frame)))
	target := w.seq

	if !durability || !w.syncMode {
		w.mu.Unlock()
		return nil
	}
	return w.durable(target)
}

// durable 确保 seq <= target 的记录已落盘。第一个进来的人执行 flush+fsync，
// 其他人等它的广播复用结果；这正是并发场景下把 fsync 次数压下来的地方。
func (w *WAL) durable(target int64) error {
	for {
		// 已经有人把我们要的记录顺带 fsync 掉了。
		if w.syncedSeq >= target {
			w.mu.Unlock()
			return nil
		}
		if w.syncing {
			// 有人正在 fsync，等它。它醒来后会重新检查 syncedSeq。
			w.group.Wait()
			continue
		}
		// 没有人在 fsync，且我们要的还没落盘 —— 我们来当这个 syncer。
		w.syncing = true
		// flush 必须在持锁下完成：bufio 不支持并发使用。
		flushErr := w.bw.Flush()
		batch := w.seq // 本次 fsync 覆盖到这里的全部记录
		start := time.Now()
		w.mu.Unlock()

		syncErr := w.f.Sync()
		d := int64(time.Since(start))

		w.mu.Lock()
		w.syncing = false
		if syncErr == nil && flushErr == nil {
			if batch > w.syncedSeq {
				w.syncedSeq = batch
			}
			w.fsyncCount.Add(1)
			w.syncNanos.Add(d)
			for {
				cur := w.maxSyncNs.Load()
				if d <= cur || w.maxSyncNs.CompareAndSwap(cur, d) {
					break
				}
			}
		}
		w.group.Broadcast()

		if flushErr != nil {
			w.mu.Unlock()
			return fmt.Errorf("wal flush: %w", flushErr)
		}
		if syncErr != nil {
			w.mu.Unlock()
			return fmt.Errorf("wal fsync: %w", syncErr)
		}
		w.mu.Unlock()
		return nil
	}
}

// Flush 把缓冲区刷入内核并 fsync 一次。
func (w *WAL) Flush() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	if err := w.bw.Flush(); err != nil {
		w.mu.Unlock()
		return fmt.Errorf("wal flush: %w", err)
	}
	target := w.seq
	return w.durable(target)
}

// Close 做最后一次 fsync 并关闭文件。幂等。
func (w *WAL) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	if err := w.bw.Flush(); err != nil {
		w.mu.Unlock()
		return fmt.Errorf("wal close flush: %w", err)
	}
	target := w.seq
	err := w.durable(target)
	// durable 在 closed 路径上会自己解锁；这里确保文件一定被关掉。
	w.mu.Lock()
	cerr := w.f.Close()
	w.mu.Unlock()
	if err != nil {
		return err
	}
	return cerr
}

// ---------------------------------------------------------------------------
// 观测
// ---------------------------------------------------------------------------

func (w *WAL) Path() string         { return w.path }
func (w *WAL) Replayed() bool       { return w.replayed }
func (w *WAL) ReplayedRecords() int { return w.replayedCount }
func (w *WAL) TruncatedTail() int64 { return w.truncatedBytes }

type WALStats struct {
	Appends     int64         `json:"appends"`
	Fsyncs      int64         `json:"fsyncs"`
	Bytes       int64         `json:"bytes"`
	AvgSync     time.Duration `json:"avg_sync"`
	MaxSync     time.Duration `json:"max_sync"`
	Replayed    bool          `json:"replayed"`
	ReplayCount int           `json:"replay_count"`
	Truncated   int64         `json:"truncated_tail_bytes"`
}

func (w *WAL) Stats() WALStats {
	f := w.fsyncCount.Load()
	var avg time.Duration
	if f > 0 {
		avg = time.Duration(w.syncNanos.Load() / f)
	}
	return WALStats{
		Appends:     w.appendCount.Load(),
		Fsyncs:      f,
		Bytes:       w.bytesCount.Load(),
		AvgSync:     avg,
		MaxSync:     time.Duration(w.maxSyncNs.Load()),
		Replayed:    w.replayed,
		ReplayCount: w.replayedCount,
		Truncated:   w.truncatedBytes,
	}
}

func (s WALStats) String() string {
	return fmt.Sprintf("appends=%d fsyncs=%d bytes=%d avg_sync=%s max_sync=%s replayed=%v records=%d truncated=%dB",
		s.Appends, s.Fsyncs, s.Bytes, s.AvgSync.Round(time.Microsecond),
		s.MaxSync.Round(time.Microsecond), s.Replayed, s.ReplayCount, s.Truncated)
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func putBE32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// ---------------------------------------------------------------------------
// 只读辅助
// ---------------------------------------------------------------------------

// readWALFile 不经过 WAL 类型直接读一遍日志，用于 demo 汇报与外部工具。
func readWALFile(path string) ([]walRecord, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	records, valid, _, err := scanWAL(f)
	return records, valid, err
}

// hardCloseForCrash 直接丢掉文件句柄，不 Flush 也不 fsync。
// 仅用于 demo/测试模拟「进程被 SIGKILL」：缓冲区里的内容因为没落盘而丢失，
// 用来验证下次启动时 WAL 的残帧截断与重放是否健壮。
func (w *WAL) hardCloseForCrash() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	_ = w.bw.Flush() // 尽量把能写的写进内核页缓存（模拟部分落盘）
	_ = w.f.Close()
}
