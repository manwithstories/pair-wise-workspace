// Package provenance 记录每项结果的来源字节范围、轨道、时间范围、
// 错误与输出 SHA-256，支撑损坏样例的首个错误定位与恢复边界验证。
package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// Record 为一条溯源记录。
type Record struct {
	Step       string   `json:"step"`
	Track      string   `json:"track"`
	SourceFile string   `json:"source_file"`
	ByteStart  int64    `json:"byte_start"` // 来源字节范围（含），-1 表示不适用
	ByteEnd    int64    `json:"byte_end"`   // 来源字节范围（不含）
	TicksStart int64    `json:"ticks_start"`
	TicksEnd   int64    `json:"ticks_end"`
	Errors     []string `json:"errors,omitempty"`
	SHA256     string   `json:"sha256"` // 该记录输出的校验和
}

// Log 为溯源记录集。
type Log struct {
	Records []Record `json:"records"`
}

// Add 追加记录；output 为产物字节，用于计算 SHA-256。
func (l *Log) Add(r Record, output []byte) {
	sum := sha256.Sum256(output)
	r.SHA256 = hex.EncodeToString(sum[:])
	l.Records = append(l.Records, r)
}

// FirstError 返回最早的字节偏移及所在记录；无错误返回 -1。
func (l *Log) FirstError() (int64, *Record) {
	best := int64(-1)
	var rec *Record
	for i := range l.Records {
		r := &l.Records[i]
		if len(r.Errors) == 0 || r.ByteStart < 0 {
			continue
		}
		if best < 0 || r.ByteStart < best {
			best = r.ByteStart
			rec = r
		}
	}
	return best, rec
}

// Save 原子写入溯源记录。
func (l *Log) Save(path string) error {
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load 读取溯源记录；不存在返回空 Log。
func Load(path string) (*Log, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if os.IsNotExist(err) {
		return &Log{}, nil
	}
	if err != nil {
		return nil, err
	}
	var l Log
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	return &l, nil
}
