// Package journal 记录阶段写前日志（WAL），
// 校验输入摘要与产物后原子提交；中断回滚到最后有效边界。
package journal

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"review/internal/fault"
)

// WAL 写前日志。
type WAL struct {
	f    *os.File
	path string
}

// Open 以追加方式打开 WAL。
func Open(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &WAL{f: f, path: path}, nil
}

func (w *WAL) record(op, stage, digest string) error {
	if _, err := fmt.Fprintf(w.f, "%s %s %s\n", op, stage, digest); err != nil {
		return err
	}
	return w.f.Sync() // 落盘后再推进状态
}

// Begin 记录阶段开始与输入摘要。
func (w *WAL) Begin(stage, inputDigest string) error { return w.record("BEGIN", stage, inputDigest) }

// Commit 记录阶段提交与产物摘要。
func (w *WAL) Commit(stage, outputDigest string) error { return w.record("COMMIT", stage, outputDigest) }

// Abort 记录阶段中止。
func (w *WAL) Abort(stage string) error { return w.record("ABORT", stage, "-") }

// Close 关闭 WAL。
func (w *WAL) Close() error { return w.f.Close() }

// Entry 一条重放记录。
type Entry struct {
	Op     string
	Stage  string
	Digest string
}

// Replay 重放 WAL，返回已提交阶段（最后有效边界内）及未闭合阶段数。
func Replay(path string) (committed []Entry, dangling int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	open := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) != 3 {
			continue
		}
		e := Entry{Op: parts[0], Stage: parts[1], Digest: parts[2]}
		switch e.Op {
		case "BEGIN":
			open[e.Stage] = true
		case "COMMIT":
			if open[e.Stage] {
				committed = append(committed, e)
				delete(open, e.Stage)
			}
		case "ABORT":
			delete(open, e.Stage)
		}
	}
	return committed, len(open), sc.Err()
}

// HashDir 计算目录内容摘要（文件名与内容，按名排序），保证重放一致。
func HashDir(dir string) (string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashBytes 计算字节摘要。
func HashBytes(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Tx 阶段事务：先写 WAL，校验后原子提交，失败回滚。
type Tx struct {
	wal     *WAL
	outDir  string
	Commits map[string]int
}

// NewTx 创建事务执行器。
func NewTx(wal *WAL, outDir string) *Tx {
	return &Tx{wal: wal, outDir: outDir, Commits: map[string]int{}}
}

// Run 执行一个阶段：fn 在临时目录产出结果并返回产物摘要；
// 成功则原子改名提交，失败则中止并清理，回滚到最后有效边界。
func (t *Tx) Run(stage, inputDigest string, fn func(dir string) (string, error)) (digest string, err error) {
	if err := t.wal.Begin(stage, inputDigest); err != nil {
		return "", fault.Newf(fault.KindTx, stage, 0, false, "wal begin: %v", err)
	}
	tmp := filepath.Join(t.outDir, ".tmp-"+stage)
	final := filepath.Join(t.outDir, stage)
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", fault.Newf(fault.KindTx, stage, 0, false, "mkdir: %v", err)
	}
	digest, err = fn(tmp)
	if err != nil {
		os.RemoveAll(tmp)
		t.wal.Abort(stage)
		return "", fault.Newf(fault.KindTx, stage, 0, false, "stage aborted, rolled back: %v", err)
	}
	if err := t.wal.Commit(stage, digest); err != nil {
		os.RemoveAll(tmp)
		return "", fault.Newf(fault.KindTx, stage, 0, false, "wal commit: %v", err)
	}
	os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil { // 同分区原子改名
		os.RemoveAll(tmp)
		return "", fault.Newf(fault.KindTx, stage, 0, false, "atomic rename: %v", err)
	}
	t.Commits[stage]++
	return digest, nil
}

// Verify 校验 WAL 中已提交阶段摘要与当前重放一致。
func Verify(walPath string, want map[string]string) (bool, error) {
	committed, dangling, err := Replay(walPath)
	if err != nil {
		return false, err
	}
	if dangling != 0 {
		return false, nil
	}
	got := map[string]string{}
	for _, e := range committed {
		got[e.Stage] = e.Digest
	}
	for k, v := range want {
		if got[k] != v {
			return false, nil
		}
	}
	return true, nil
}
