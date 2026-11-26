// Package journal 维护归档流水线的检查点、幂等键与溯源记录。
//
// 提交纪律（对应需求 5）：
//   - 片段文件与日志条目必须先落盘，检查点才允许提交；
//   - 所有写操作走“临时文件 + 原子 rename”，崩溃后状态只有“旧版/新版”两种；
//   - 幂等键由输入摘要、参数与适配器版本派生，重复运行得到相同键与产物哈希；
//   - 所有路径限制在用户指定根目录内，拒绝路径穿越。
package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"archivemedia/internal/model"
)

const (
	dirName        = ".archive-journal"
	checkpointFile = "checkpoint.json"
	logFile        = "events.log"
)

// Status 是片段处理状态。
type Status string

const (
	StatusPending     Status = "pending"
	StatusCommitted   Status = "committed"
	StatusInterrupted Status = "interrupted"
	StatusCorrupt     Status = "corrupt"
)

// SegmentRecord 是一个片段的溯源记录。
type SegmentRecord struct {
	SegmentID      string            `json:"segment_id"`
	SourceID       string            `json:"source_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	AdapterName    string            `json:"adapter_name"`
	AdapterVersion string            `json:"adapter_version"`
	Status         Status            `json:"status"`
	FilePath       string            `json:"file_path"`
	FileSha256     string            `json:"file_sha256"`
	StartUs        int64             `json:"start_us"`
	EndUs          int64             `json:"end_us"`
	Params         map[string]string `json:"params"`
	Provenance     string            `json:"provenance_sha256"`
}

// Checkpoint 是可续跑的全量状态。
type Checkpoint struct {
	RunID       string                    `json:"run_id"`
	UpdatedAt   string                    `json:"updated_at"`
	Segments    map[string]*SegmentRecord `json:"segments"`
	Order       []string                  `json:"order"` // 确定性提交顺序
	InterruptAt int                       `json:"interrupt_after"`
}

// Store 是日志存储，串行化所有写操作。
type Store struct {
	root string
	mu   sync.Mutex
	cp   *Checkpoint
}

// Open 在 root 下打开（或初始化）日志存储，并校验路径安全。
func Open(root string) (*Store, error) {
	abs, err := SafePath(root, ".")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(abs, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.Wrap(model.KindPathViolation, err, "journal: mkdir %s", dir)
	}
	s := &Store{root: abs}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Root 返回已解析的绝对根目录。
func (s *Store) Root() string { return s.root }

func (s *Store) path() string { return filepath.Join(s.root, dirName, checkpointFile) }

func (s *Store) load() error {
	s.cp = &Checkpoint{Segments: map[string]*SegmentRecord{}, Order: []string{}}
	data, err := os.ReadFile(s.path())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return model.Wrap(model.KindCheckpoint, err, "journal: read checkpoint")
	}
	if err := json.Unmarshal(data, s.cp); err != nil {
		return model.Wrap(model.KindCheckpoint, err, "journal: corrupt checkpoint")
	}
	if s.cp.Segments == nil {
		s.cp.Segments = map[string]*SegmentRecord{}
	}
	return nil
}

// persist 原子写检查点。
func (s *Store) persist() error {
	data, err := json.MarshalIndent(s.cp, "", "  ")
	if err != nil {
		return model.Wrap(model.KindCheckpoint, err, "journal: marshal")
	}
	final := s.path()
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return model.Wrap(model.KindCheckpoint, err, "journal: write tmp")
	}
	if err := os.Rename(tmp, final); err != nil {
		return model.Wrap(model.KindCheckpoint, err, "journal: rename")
	}
	return nil
}

// IdempotencyKey 由输入摘要、参数与适配器版本确定性派生。
func IdempotencyKey(inputDigest string, params map[string]string, adapterName, adapterVersion string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("v1|")
	b.WriteString(adapterName)
	b.WriteString("@")
	b.WriteString(adapterVersion)
	b.WriteString("|")
	b.WriteString(inputDigest)
	b.WriteString("|")
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(params[k])
		b.WriteString(";")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ProvenanceHash 计算单条记录的溯源哈希（含来源与产物哈希）。
func ProvenanceHash(r *SegmentRecord) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		r.SegmentID, r.SourceID, r.IdempotencyKey, r.AdapterName, r.AdapterVersion,
		r.FilePath, r.FileSha256, fmt.Sprintf("%d", r.StartUs), fmt.Sprintf("%d", r.EndUs),
	}, "|")))
	return hex.EncodeToString(sum[:])
}

// Prepare 登记一个待提交片段（pending），不触发检查点提交。
// 同幂等键已存在（无论 committed 还是中断遗留的 pending）都返回既有记录，
// 不重复追加 Order，保证检查点中每个键只出现一次。
func (s *Store) Prepare(r *SegmentRecord) (*SegmentRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.cp.Segments[r.IdempotencyKey]; ok {
		return existing, true, nil
	}
	r.Status = StatusPending
	s.cp.Segments[r.IdempotencyKey] = r
	s.cp.Order = append(s.cp.Order, r.IdempotencyKey)
	return r, false, nil
}

// Get 只读返回任意状态的片段记录（committed 或中断遗留的 pending）。
func (s *Store) Get(key string) (*SegmentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.cp.Segments[key]
	return r, ok
}

// RecordCount 返回检查点中不同幂等键的数量，用于续跑序号初始化。
func (s *Store) RecordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cp.Segments)
}

// Commit 在片段文件确认落盘后提交检查点，并追加溯源事件。
func (s *Store) Commit(key string, fileSha256 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.cp.Segments[key]
	if !ok {
		return model.NewError(model.KindCheckpoint, "journal: unknown key %s", key)
	}
	// 提交前置条件：片段文件必须真实存在且哈希匹配。
	got, err := HashFile(filepath.Join(s.root, rec.FilePath))
	if err != nil {
		return model.Wrap(model.KindCheckpoint, err, "journal: segment file missing before commit")
	}
	if got != fileSha256 {
		return model.NewError(model.KindCheckpoint,
			"journal: hash mismatch want=%s got=%s", fileSha256, got)
	}
	rec.FileSha256 = fileSha256
	rec.Status = StatusCommitted
	rec.Provenance = ProvenanceHash(rec)
	if err := s.appendEvent(rec); err != nil {
		return err
	}
	return s.persist()
}

// MarkInterrupted 记录模拟中断点（已提交片段不受影响，可续跑）。
func (s *Store) MarkInterrupted(after int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cp.InterruptAt = after
	return s.persist()
}

// Committed 返回已提交片段（按确定性顺序）。
func (s *Store) Committed() []*SegmentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*SegmentRecord
	for _, k := range s.cp.Order {
		if r := s.cp.Segments[k]; r != nil && r.Status == StatusCommitted {
			out = append(out, r)
		}
	}
	return out
}

// Pending 返回尚未提交的片段键。
func (s *Store) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, k := range s.cp.Order {
		if r := s.cp.Segments[k]; r != nil && r.Status == StatusPending {
			out = append(out, k)
		}
	}
	return out
}

// HasCommitted 只读判断幂等键是否已提交，不改变检查点状态。
func (s *Store) HasCommitted(key string) (*SegmentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.cp.Segments[key]
	if !ok || r.Status != StatusCommitted {
		return nil, false
	}
	return r, true
}

// Checkpoint 导出当前快照（核验模块只读）。
func (s *Store) Checkpoint() Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *s.cp
	cp.Segments = make(map[string]*SegmentRecord, len(s.cp.Segments))
	for k, v := range s.cp.Segments {
		cp.Segments[k] = v
	}
	cp.Order = append([]string(nil), s.cp.Order...)
	return cp
}

func (s *Store) appendEvent(r *SegmentRecord) error {
	f, err := os.OpenFile(filepath.Join(s.root, dirName, logFile),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return model.Wrap(model.KindCheckpoint, err, "journal: open log")
	}
	defer f.Close()
	line, _ := json.Marshal(r)
	if _, err := f.Write(append(line, '\n')); err != nil {
		return model.Wrap(model.KindCheckpoint, err, "journal: append log")
	}
	return nil
}

// HashFile 返回文件的 sha256 十六进制摘要。
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", model.Wrap(model.KindCorruptMedia, err, "journal: hash read %s", path)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// AggregateProvenance 把多个片段溯源哈希排序后再聚合为运行级 provenance。
// 排序保证与片段顺序无关、重复运行结果一致。
func AggregateProvenance(prov []string) string {
	sorted := append([]string(nil), prov...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, p := range sorted {
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SafePath 把 name 解析为 root 内的绝对路径，拒绝任何越界（路径穿越）。
func SafePath(root, name string) (string, error) {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", model.Wrap(model.KindPathViolation, err, "safe path: root")
	}
	// 拒绝绝对路径形式的 name，强制其相对 root。
	if filepath.IsAbs(name) {
		return "", model.NewError(model.KindPathViolation, "safe path: absolute name rejected: %s", name)
	}
	joined := filepath.Clean(filepath.Join(absRoot, name))
	rel, err := filepath.Rel(absRoot, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", model.NewError(model.KindPathViolation,
			"safe path: %q escapes root %q", name, absRoot)
	}
	return joined, nil
}
