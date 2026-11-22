// Package model 定义媒体流水线的核心领域对象：来源、轨道、样本与片段。
//
// 所有结构体都带 JSON 标签，可往返序列化；时间字段统一使用所在轨道
// 时基下的整数 tick（允许负值以表达负时间戳样本）。
package model

import (
	"encoding/json"
	"fmt"

	"mediad/internal/clock"
)

// MaxSegmentBytes 限制单个输出片段最大 64 MiB，超限必须拒绝。
const MaxSegmentBytes = 64 * 1024 * 1024

// 输入来源类型。
const (
	KindMP4 = "mp4"
	KindRTP = "rtp"
)

// 片段处理状态。
const (
	// StatusOK 片段完整，所有来源样本均可解析。
	StatusOK = "ok"
	// StatusIncomplete 剪辑点落在截断/损坏区间，片段数据不完整。
	StatusIncomplete = "incomplete"
	// StatusCorrupt 片段包含损坏样本盒。
	StatusCorrupt = "corrupt"
	// StatusTruncated 片段对应的数据在声明长度前被截断。
	StatusTruncated = "truncated"
)

// Source 描述一个输入素材及其内容摘要。
type Source struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Digest string `json:"digest"` // 整个输入文件的 SHA-256（十六进制）
	// RTPClock 仅 RTP 来源使用，单位为“每秒钟 tick 数”。
	RTPClock clock.Rational `json:"rtp_clock,omitempty"`
}

// Track 描述一条轨道及其时间基准。
type Track struct {
	ID       string         `json:"id"`
	SourceID string         `json:"source_id"`
	Handler  string         `json:"handler"` // vide / soun
	Timebase clock.Rational `json:"timebase"`
	// Timescale 是 Timebase.Num 的便捷表示（MP4 mdhd.timescale）。
	Timescale uint32 `json:"timescale"`
}

// Sample 是轨道上的单个样本，Index 遵循 MP4 的 1 基约定。
type Sample struct {
	TrackID   string `json:"track_id"`
	Index     uint32 `json:"index"`
	PTS       int64  `json:"pts"` // 轨道时基 tick，允许负值
	Duration  uint32 `json:"duration"`
	Offset    uint64 `json:"offset"` // 输入文件内字节偏移
	Size      uint32 `json:"size"`
	Keyframe  bool   `json:"keyframe"`
	Complete  bool   `json:"complete"` // false 表示截断或损坏样本
}

// SourceRef 记录片段引用的某个来源上的样本区间（1 基，闭开区间）。
type SourceRef struct {
	SourceID      string `json:"source_id"`
	TrackID       string `json:"track_id"`
	FirstSample   uint32 `json:"first_sample"`
	LastSample    uint32 `json:"last_sample"`
	StartTick     int64  `json:"start_tick"`
	EndTick       int64  `json:"end_tick"`
}

// Segment 是切分产物。转场片段可持有多条 SourceRef 实现跨轨引用。
type Segment struct {
	ID        string      `json:"id"`
	TrackID   string      `json:"track_id"`
	StartTick int64       `json:"start_tick"`
	EndTick   int64       `json:"end_tick"`
	Refs      []SourceRef `json:"refs"`
	Status    string      `json:"status"`
	ByteLen   int         `json:"byte_len"`
	// ContentDigest 是片段字节+时间范围的内容指纹（十六进制）。
	ContentDigest string `json:"content_digest,omitempty"`
	// SourceDigest 是来源摘要的组合，缓存命中时必须复核。
	SourceDigest string `json:"source_digest,omitempty"`
}

// Complete 报告片段是否处于完整可用状态。
func (s *Segment) Complete() bool { return s.Status == StatusOK }

// Manifest 是一次流水线运行的完整清单，也是检查点持久化的主体。
type Manifest struct {
	Version  int       `json:"version"`
	InputID  string    `json:"input_id"` // 由全部输入摘要派生，变化即失效
	Sources  []Source  `json:"sources"`
	Tracks   []Track   `json:"tracks"`
	Samples  []Sample  `json:"samples"`
	Segments []Segment `json:"segments"`
}

// SegmentByID 按 ID 查找片段，未找到返回 nil。
func (m *Manifest) SegmentByID(id string) *Segment {
	for i := range m.Segments {
		if m.Segments[i].ID == id {
			return &m.Segments[i]
		}
	}
	return nil
}

// TrackByID 按 ID 查找轨道，未找到返回 nil。
func (m *Manifest) TrackByID(id string) *Track {
	for i := range m.Tracks {
		if m.Tracks[i].ID == id {
			return &m.Tracks[i]
		}
	}
	return nil
}

// ValidateSegmentBytes 拒绝超过 64 MiB 的片段。
func ValidateSegmentBytes(n int) error {
	if n < 0 {
		return fmt.Errorf("model: negative segment size %d", n)
	}
	if n > MaxSegmentBytes {
		return fmt.Errorf("model: segment %d bytes exceeds limit %d", n, MaxSegmentBytes)
	}
	return nil
}

// Roundtrip 序列化后反序列化清单，用于验证字段一致性。
func (m *Manifest) Roundtrip() (*Manifest, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("model: marshal manifest: %w", err)
	}
	var out Manifest
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("model: unmarshal manifest: %w", err)
	}
	return &out, nil
}
