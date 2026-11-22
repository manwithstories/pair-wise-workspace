// Package parse 负责容错读取 ISO BMFF（MP4/MOV）盒结构，并解析跨轨定时
// 元数据。所有解析问题统一归入三级错误模型：
//
//   - Recoverable：局部损坏，已跳过或截断，后续内容继续解析
//   - Skipped：不支持或被安全限制跳过的盒，不影响其余素材
//   - Fatal：输入无法作为容器继续处理（如缺失 ftyp/moov）
package parse

import "fmt"

// Level 是错误级别。
type Level string

const (
	LevelRecoverable Level = "recoverable"
	LevelSkipped     Level = "skipped"
	LevelFatal       Level = "fatal"
)

// Error 携带级别、位置与原始上下文，映射失败类问题还会保留原始时间戳。
type Error struct {
	Level   Level
	BoxType string
	Offset  int64 // 损坏发生的文件偏移，-1 表示不适用
	TrackID uint32
	RawTick uint64
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("parse %s: box=%s offset=%d track=%d: %s",
		e.Level, e.BoxType, e.Offset, e.TrackID, e.Message)
}

// Limits 限定解析资源占用，防止恶意盒长度导致内存耗尽。
type Limits struct {
	MaxFileSize  int64
	MaxBoxes     int
	MaxDepth     int
	MaxSampleN   int
	MaxPayload   int64 // 单个盒允许读入内存的最大净荷
	MaxEvents    int
}

// DefaultLimits 为现场素材的保守上限。
func DefaultLimits() Limits {
	return Limits{
		MaxFileSize: 2 << 30, // 2 GiB
		MaxBoxes:    200_000,
		MaxDepth:    32,
		MaxSampleN:  500_000,
		MaxPayload:  64 << 20, // 64 MiB
		MaxEvents:   100_000,
	}
}

// Boundary 记录一次截断或坏片段的边界信息，供恢复与审计使用。
type Boundary struct {
	Offset   int64  `json:"offset"`
	BoxType  string `json:"box_type"`
	Kind     string `json:"kind"` // truncated | bad_box | oversized | depth
	Level    Level  `json:"level"`
	Message  string `json:"message"`
	Recovered int64 `json:"recovered_bytes"` // 该边界之前成功保留的字节
}

// Report 汇总一次容器解析的健康状况。
type Report struct {
	Errors      []*Error   `json:"errors"`
	Boundaries  []Boundary `json:"boundaries"`
	BoxesSeen   int        `json:"boxes_seen"`
	BoxesParsed int        `json:"boxes_parsed"`
	SkippedBoxes int       `json:"skipped_boxes"`
	RecoveredBytes int64   `json:"recovered_bytes"`
	Truncated   bool      `json:"truncated"`
}

func (r *Report) add(e *Error) {
	r.Errors = append(r.Errors, e)
	switch e.Level {
	case LevelSkipped:
		r.SkippedBoxes++
	case LevelRecoverable:
		r.RecoveredBytes += 0 // 具体保留字节由边界记录累加
	}
}

// AddError 供外部子系统追加一条带级别的解析错误。
func (r *Report) AddError(e *Error) { r.add(e) }

// HasFatal 报告是否出现致命错误。
func (r *Report) HasFatal() bool {
	for _, e := range r.Errors {
		if e.Level == LevelFatal {
			return true
		}
	}
	return false
}

// TimelineErrors 统计与时间线/时间戳相关的错误数。
func (r *Report) TimelineErrors() int {
	n := 0
	for _, e := range r.Errors {
		if e.BoxType == "timestamp" || e.BoxType == "mdhd" || e.BoxType == "stts" {
			n++
		}
	}
	return n
}
