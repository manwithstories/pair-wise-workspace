// Package model 定义多轨时间线、剪辑、转场以及统一媒体时间基准。
// 所有跨轨道的时间换算都通过整数乘除完成，避免浮点累计误差。
package model

import (
	"errors"
	"fmt"
	"math"
)

// UnifiedTimescale 是统一时间基准：每秒 1_000_000 tick（微秒级）。
const UnifiedTimescale uint64 = 1_000_000

// 输入上限约束：超出即拒绝，防止溢出与资源耗尽。
const (
	MaxTracks        = 16
	MaxSamplesPerRun = 1 << 20
	MaxSampleSize    = 4 << 20 // 4 MiB
	MaxClipCount     = 4096
	MaxTimescale     = 90_000_000
)

// ErrOverflow 表示时间基准换算或输入长度溢出。
var ErrOverflow = errors.New("model: 时间基准或长度溢出")

// TrackKind 区分轨道类型。
type TrackKind string

const (
	TrackVideo TrackKind = "video"
	TrackAudio TrackKind = "audio"
)

// Track 描述一条媒体轨道及其时钟频率。
type Track struct {
	ID        uint32
	Kind      TrackKind
	Timescale uint32 // 每秒 tick 数
	Name      string
}

// Clip 是时间线上的一段连续素材，时间以所属轨道时钟计。
type Clip struct {
	ID           string
	TrackID      uint32
	Index        int    // 在轨道上的顺序
	StartTick    uint64 // 轨道时钟起点
	DurationTick uint64 // 轨道时钟长度
}

// Transition 描述两个剪辑之间的转场片段。
type Transition struct {
	ID           string
	TrackID      uint32
	FromClipID   string
	ToClipID     string
	DurationTick uint64 // 轨道时钟长度
}

// Timeline 聚合轨道、剪辑与转场。
type Timeline struct {
	Tracks      []Track
	Clips       []Clip
	Transitions []Transition
}

// Validate 检查时间线是否满足上限与引用完整性。
func (tl *Timeline) Validate() error {
	if len(tl.Tracks) == 0 || len(tl.Tracks) > MaxTracks {
		return fmt.Errorf("model: 轨道数 %d 超出上限 [1,%d]", len(tl.Tracks), MaxTracks)
	}
	if len(tl.Clips) > MaxClipCount || len(tl.Transitions) > MaxClipCount {
		return fmt.Errorf("model: 剪辑/转场数量超出上限 %d", MaxClipCount)
	}
	trackByID := make(map[uint32]Track, len(tl.Tracks))
	for _, t := range tl.Tracks {
		if t.Timescale == 0 || t.Timescale > MaxTimescale {
			return fmt.Errorf("model: 轨道 %d 时钟频率 %d 非法", t.ID, t.Timescale)
		}
		trackByID[t.ID] = t
	}
	clipByID := make(map[string]bool, len(tl.ClipID()))
	for _, c := range tl.Clips {
		if _, ok := trackByID[c.TrackID]; !ok {
			return fmt.Errorf("model: 剪辑 %s 引用未知轨道 %d", c.ID, c.TrackID)
		}
		clipByID[c.ID] = true
	}
	for _, tr := range tl.Transitions {
		if !clipByID[tr.FromClipID] || !clipByID[tr.ToClipID] {
			return fmt.Errorf("model: 转场 %s 引用未知剪辑", tr.ID)
		}
	}
	return nil
}

// ClipID 返回所有剪辑 ID（供校验使用）。
func (tl *Timeline) ClipID() []string {
	ids := make([]string, 0, len(tl.Clips))
	for _, c := range tl.Clips {
		ids = append(ids, c.ID)
	}
	return ids
}

// TrackByID 查找轨道。
func (tl *Timeline) TrackByID(id uint32) (Track, bool) {
	for _, t := range tl.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return Track{}, false
}

// ToUnified 把轨道时钟 tick 换算到统一时间基准，纯整数运算。
// 换算公式 tick * UnifiedTimescale / timescale，先除公因数再乘，溢出时报错。
func ToUnified(timescale uint32, tick uint64) (uint64, error) {
	if timescale == 0 || timescale > MaxTimescale {
		return 0, fmt.Errorf("%w: 非法时钟频率 %d", ErrOverflow, timescale)
	}
	ts := uint64(timescale)
	g := gcd(UnifiedTimescale, ts)
	mul := UnifiedTimescale / g
	div := ts / g
	if tick > math.MaxUint64/mul {
		return 0, fmt.Errorf("%w: tick=%d timescale=%d", ErrOverflow, tick, timescale)
	}
	return tick / div * mul + (tick%div)*mul/div, nil
}

// MustUnified 是 ToUnified 的便捷封装，溢出时返回 0（仅用于已校验输入）。
func MustUnified(timescale uint32, tick uint64) uint64 {
	v, err := ToUnified(timescale, tick)
	if err != nil {
		return 0
	}
	return v
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Sample 是一个媒体样本，保留原始时间戳供审计。
type Sample struct {
	TrackID      uint32
	Seq          uint64 // 轨道内样本序号
	DTS          uint64 // 原始解码时间戳（轨道时钟）
	CTS          uint64 // 原始显示时间戳（轨道时钟）
	Duration     uint32 // 轨道时钟长度
	Keyframe     bool
	SourceOffset int64 // 输入文件中的原始字节偏移
	Data         []byte // 样本负载（尺寸受 MaxSampleSize 约束）
}

// RepairedSample 携带修复后的统一时间基准时间戳与审计信息。
type RepairedSample struct {
	Sample
	UnifiedDTS   uint64 // 修复后统一时间基准 DTS
	UnifiedCTS   uint64
	OriginalDTS  uint64 // 修复前原始值，供审计
	OriginalCTS  uint64
	RepairFlags  RepairFlag
	SegmentID    string // 所属剪辑或转场 ID
	IsTransition bool
}

// RepairFlag 标记样本经历了哪些修复。
type RepairFlag uint32

const (
	RepairNone       RepairFlag = 0
	RepairWrap       RepairFlag = 1 << 0 // 时间戳回绕
	RepairDuplicate  RepairFlag = 1 << 1 // 重复时间戳
	RepairRegression RepairFlag = 1 << 2 // 时间戳倒退
	RepairClipJump   RepairFlag = 1 << 3 // 跨剪辑跳变
	RepairDrift      RepairFlag = 1 << 4 // 时钟漂移
)

func (f RepairFlag) String() string {
	if f == RepairNone {
		return "none"
	}
	out := ""
	add := func(flag RepairFlag, name string) {
		if f&flag != 0 {
			if out != "" {
				out += "|"
			}
			out += name
		}
	}
	add(RepairWrap, "wrap")
	add(RepairDuplicate, "duplicate")
	add(RepairRegression, "regression")
	add(RepairClipJump, "clip_jump")
	add(RepairDrift, "drift")
	return out
}
