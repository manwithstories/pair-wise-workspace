package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// OutputRow 是两条导出路径共享的行模型；校正值在进入导出前已由
// timebase.go 的同一函数换算，JSON 与 CSV 逐项严格一致。
type OutputRow struct {
	Track        string
	Segment      int // 段号：按 epoch 升序从 0 编号
	Epoch        int64
	Seq          int64
	OriginalPTS  int64
	CorrectedPTS int64
	SyncID       string
	HasSync      bool
}

// jsonRow 定义磁盘上的字段顺序与命名。
type jsonRow struct {
	Track        string `json:"track"`
	Segment      int    `json:"segment"`
	Epoch        int64  `json:"epoch"`
	Seq          int64  `json:"seq"`
	OriginalPTS  int64  `json:"original_pts"`
	CorrectedPTS int64  `json:"corrected_pts"`
	SyncID       string `json:"sync_id,omitempty"`
}

// JSONWriter 以流式数组写出，避免百万行整体驻留。
type JSONWriter struct {
	path string
	tmp  *os.File
	enc  *json.Encoder
	n    int
}

func NewJSONWriter(path string) (*JSONWriter, error) {
	tmp, err := createTemp(path)
	if err != nil {
		return nil, err
	}
	w := &JSONWriter{path: path, tmp: tmp, enc: json.NewEncoder(tmp)}
	if _, err := tmp.WriteString("[\n"); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	return w, nil
}

func (w *JSONWriter) Write(row OutputRow) error {
	if w.n > 0 {
		if _, err := w.tmp.WriteString(",\n"); err != nil {
			return err
		}
	}
	jr := jsonRow{
		Track: row.Track, Segment: row.Segment, Epoch: row.Epoch, Seq: row.Seq,
		OriginalPTS: row.OriginalPTS, CorrectedPTS: row.CorrectedPTS,
	}
	if row.HasSync {
		jr.SyncID = row.SyncID
	}
	if err := w.enc.Encode(jr); err != nil {
		return err
	}
	w.n++
	return nil
}

func (w *JSONWriter) Close() error {
	if _, err := w.tmp.WriteString("\n]\n"); err != nil {
		w.Abort()
		return err
	}
	if err := w.tmp.Close(); err != nil {
		os.Remove(w.tmp.Name())
		return err
	}
	return os.Rename(w.tmp.Name(), w.path)
}

// Abort 关闭并删除临时文件；已 rename 的文件不受影响。
func (w *JSONWriter) Abort() {
	if w == nil || w.tmp == nil {
		return
	}
	name := w.tmp.Name()
	w.tmp.Close()
	os.Remove(name)
}

// CSVWriter 带表头，全部数值以十进制文本写出。
type CSVWriter struct {
	path string
	tmp  *os.File
	bw   *bufio.Writer
	cw   *csv.Writer
	n    int
}

var csvHeader = []string{"track", "segment", "epoch", "seq", "original_pts", "corrected_pts", "sync_id"}

func NewCSVWriter(path string) (*CSVWriter, error) {
	tmp, err := createTemp(path)
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(tmp, 1<<20)
	cw := csv.NewWriter(bw)
	w := &CSVWriter{path: path, tmp: tmp, bw: bw, cw: cw}
	if err := cw.Write(csvHeader); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	return w, nil
}

func (w *CSVWriter) Write(row OutputRow) error {
	rec := []string{
		row.Track,
		strconv.Itoa(row.Segment),
		strconv.FormatInt(row.Epoch, 10),
		strconv.FormatInt(row.Seq, 10),
		strconv.FormatInt(row.OriginalPTS, 10),
		strconv.FormatInt(row.CorrectedPTS, 10),
		"",
	}
	if row.HasSync {
		rec[6] = row.SyncID
	}
	if err := w.cw.Write(rec); err != nil {
		return err
	}
	w.n++
	return nil
}

func (w *CSVWriter) Close() error {
	w.cw.Flush()
	if err := w.cw.Error(); err != nil {
		w.Abort()
		return err
	}
	if err := w.bw.Flush(); err != nil {
		w.Abort()
		return err
	}
	if err := w.tmp.Close(); err != nil {
		os.Remove(w.tmp.Name())
		return err
	}
	return os.Rename(w.tmp.Name(), w.path)
}

func (w *CSVWriter) Abort() {
	if w == nil || w.tmp == nil {
		return
	}
	name := w.tmp.Name()
	w.tmp.Close()
	os.Remove(name)
}

// createTemp 在目标目录内创建临时文件，确保 rename 是同文件系统原子操作；
// 父目录缺失时一并创建。任何失败都映射为调用方的退出码 4。
func createTemp(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create output dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".avsync-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create output file: %w", err)
	}
	return tmp, nil
}
