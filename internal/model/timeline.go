// Package model 定义流水线各模块共享的采样时钟、时间线与基础数值约束。
package model

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// 安全上限：防止畸形 RIFF 长度或采样点运算导致整数溢出/资源耗尽。
const (
	// MaxFileBytes 单个 WAV 文件允许的最大字节数（4 GiB 减 1）。
	MaxFileBytes int64 = 1<<32 - 1
	// MaxSamples 单轨道允许的最大采样点数。
	MaxSamples int64 = 1 << 40
	// MaxTracks 单任务允许的最大轨道数。
	MaxTracks = 64
)

// 缺口类型常量。
const (
	GapKindLoss        = "loss"        // 突发丢包造成的缺口
	GapKindCRC         = "crc"         // CRC 校验失败丢弃的包
	GapKindTruncation  = "truncation"  // 尾块截断
	GapKindOutOfWindow = "outofwindow" // 超过乱序窗口仍未到达
)

// Clock 描述单条时间线使用的采样时钟。
type Clock struct {
	SampleRate uint32
}

// NewClock 构造时钟并拒绝非法采样率。
func NewClock(rate uint32) (*Clock, error) {
	if rate == 0 {
		return nil, fmt.Errorf("%w: 采样率不能为 0", ErrInvalidInput)
	}
	if rate > 1<<24 {
		return nil, fmt.Errorf("%w: 采样率 %d 超出合理范围", ErrInvalidInput, rate)
	}
	return &Clock{SampleRate: rate}, nil
}

// SamplesToDuration 将采样点数换算为播放时长。
func (c *Clock) SamplesToDuration(n int64) time.Duration {
	if c == nil || c.SampleRate == 0 {
		return 0
	}
	// 先转纳秒避免中间结果溢出：n * 1e9 / rate。
	ns := Mul64(n, int64(time.Second))
	return time.Duration(ns / int64(c.SampleRate))
}

// FrameBytes 返回 n 个采样帧占用的字节数，溢出时返回错误。
func (c *Clock) FrameBytes(n int64, blockAlign uint16) (int64, error) {
	if blockAlign == 0 {
		return 0, fmt.Errorf("%w: 块对齐不能为 0", ErrInvalidInput)
	}
	return Mul64Checked(n, int64(blockAlign))
}

// Gap 表示时间线上的损坏区段，采用半开采样区间 [Start, End)。
// 缺口只做标记，绝不挤移后续采样点。
type Gap struct {
	TrackID   int    `json:"track_id"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	Kind      string `json:"kind"`
	LocError  int64  `json:"loc_error_samples"`
	Recovered bool   `json:"recovered"`
}

// Length 返回缺口覆盖的采样点数。
func (g Gap) Length() int64 { return g.End - g.Start }

// Contains 判断采样点 p 是否落在缺口内。
func (g Gap) Contains(p int64) bool { return p >= g.Start && p < g.End }

// Anchor 是包记录提供的同步锚点。PCM 不存在编解码关键帧，
// 因此关键帧标记完全由包记录中的锚点位承担。
type Anchor struct {
	TrackID int   `json:"track_id"`
	Sample  int64 `json:"sample"`
	Seq     int64 `json:"seq"`
}

// AnchorInterval 描述相邻锚点之间的间隔；缺标时 Present 为 false。
type AnchorInterval struct {
	From    int64   `json:"from"`
	To      int64   `json:"to"`
	Samples int64   `json:"samples"`
	Ms      float64 `json:"ms"`
	Present bool    `json:"present"`
}

// TrackTimeline 是单条轨道的采样时间线：期望长度、缺口与锚点。
type TrackTimeline struct {
	TrackID int           `json:"track_id"`
	Clock   *Clock        `json:"-"`
	Total   int64         `json:"total_samples"`
	Gaps    []Gap         `json:"gaps"`
	Anchors []Anchor      `json:"anchors"`
}

// NewTrackTimeline 创建指定期望采样总数的轨道时间线。
func NewTrackTimeline(trackID int, clock *Clock, total int64) (*TrackTimeline, error) {
	if clock == nil {
		return nil, fmt.Errorf("%w: 时钟不能为空", ErrInvalidInput)
	}
	if err := CheckSampleCount(total); err != nil {
		return nil, err
	}
	return &TrackTimeline{TrackID: trackID, Clock: clock, Total: total}, nil
}

// AddGap 登记一个缺口并按起始采样点保持有序；与既有缺口重叠时合并。
func (tl *TrackTimeline) AddGap(g Gap) error {
	if g.Start < 0 || g.End <= g.Start || g.End > tl.Total {
		return fmt.Errorf("%w: 缺口范围 [%d,%d) 越界，轨道总采样 %d",
			ErrInvalidInput, g.Start, g.End, tl.Total)
	}
	g.TrackID = tl.TrackID
	pos := sort.Search(len(tl.Gaps), func(i int) bool { return tl.Gaps[i].Start >= g.Start })
	tl.Gaps = append(tl.Gaps, Gap{})
	copy(tl.Gaps[pos+1:], tl.Gaps[pos:])
	tl.Gaps[pos] = g
	tl.mergeOverlaps()
	return nil
}

// mergeOverlaps 合并相邻或重叠的缺口，同 Kind 时保留较大定位误差。
func (tl *TrackTimeline) mergeOverlaps() {
	if len(tl.Gaps) < 2 {
		return
	}
	merged := tl.Gaps[:1]
	for _, g := range tl.Gaps[1:] {
		last := &merged[len(merged)-1]
		if g.Start <= last.End {
			if g.End > last.End {
				last.End = g.End
			}
			if g.LocError > last.LocError {
				last.LocError = g.LocError
			}
			continue
		}
		merged = append(merged, g)
	}
	tl.Gaps = merged
}

// AddAnchor 登记同步锚点并去重、排序。
func (tl *TrackTimeline) AddAnchor(a Anchor) {
	a.TrackID = tl.TrackID
	for _, ex := range tl.Anchors {
		if ex.Sample == a.Sample {
			return
		}
	}
	tl.Anchors = append(tl.Anchors, a)
	sort.Slice(tl.Anchors, func(i, j int) bool { return tl.Anchors[i].Sample < tl.Anchors[j].Sample })
}

// AnchorIntervals 计算相邻锚点间隔；锚点不足两个时返回 nil（N/A）。
func (tl *TrackTimeline) AnchorIntervals() []AnchorInterval {
	if len(tl.Anchors) < 2 {
		return nil
	}
	out := make([]AnchorInterval, 0, len(tl.Anchors)-1)
	for i := 1; i < len(tl.Anchors); i++ {
		prev, cur := tl.Anchors[i-1], tl.Anchors[i]
		samples := cur.Sample - prev.Sample
		out = append(out, AnchorInterval{
			From:    prev.Sample,
			To:      cur.Sample,
			Samples: samples,
			Ms:      float64(samples) * 1000 / float64(tl.Clock.SampleRate),
			Present: true,
		})
	}
	return out
}

// GapSamples 返回所有缺口覆盖的采样点总数。
func (tl *TrackTimeline) GapSamples() int64 {
	var n int64
	for _, g := range tl.Gaps {
		n += g.Length()
	}
	return n
}

// CheckSampleCount 校验采样点数非负且不超过安全上限。
func CheckSampleCount(n int64) error {
	if n < 0 {
		return fmt.Errorf("%w: 采样点数为负 %d", ErrInvalidInput, n)
	}
	if n > MaxSamples {
		return fmt.Errorf("%w: 采样点数 %d 超过上限 %d", ErrInvalidInput, n, MaxSamples)
	}
	return nil
}

// Mul64 执行带溢出检查的 int64 乘法，溢出时 panic（用于已知安全的内部计算）。
func Mul64(a, b int64) int64 {
	r, err := Mul64Checked(a, b)
	if err != nil {
		panic(err)
	}
	return r
}

// Mul64Checked 执行带溢出检查的 int64 乘法，溢出时返回错误。
func Mul64Checked(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	// MinInt64 乘以绝对值大于 1 的数必然溢出，且会绕过下面的回推检查。
	if (a == math.MinInt64 || b == math.MinInt64) && !(a == 1 || b == 1 || a == -1 || b == -1) {
		return 0, fmt.Errorf("%w: 整数溢出 %d * %d", ErrInvalidInput, a, b)
	}
	r := a * b
	// 回推验证可同时捕获有符号乘法的上溢与下溢；a、b 均非零故除法安全。
	if r/a != b {
		return 0, fmt.Errorf("%w: 整数溢出 %d * %d", ErrInvalidInput, a, b)
	}
	return r, nil
}
