// Package clock 定义统一的 90 kHz 媒体时间基准、时间戳回绕常量与时间轴领域类型。
//
// 所有子系统（清单、分片、修复、报告）都以本包的 Tick 作为时间单位，
// 1 秒 = 90000 Tick。MPEG-TS / RTP 风格的 33 位时间戳回绕也在此处建模。
package clock

import (
	"errors"
	"math"
)

// TickRate 是统一媒体时间基准：90 kHz。
const TickRate uint64 = 90_000

// WrapBits 是 MPEG 风格时间戳的位宽。
const WrapBits uint = 33

// WrapMod 是 33 位回绕模数 2^33。
const WrapMod uint64 = 1 << WrapBits

// WrapHalf 是回绕空间的一半，用于判断两个相邻观测值之间哪条弧更短。
const WrapHalf uint64 = WrapMod / 2

// MaxTick 是受 33 位空间限制的原始时间戳上界（不含）。
const MaxTick uint64 = WrapMod

// 领域错误。调用方可用 errors.Is 判定错误类别。
var (
	// ErrWrapOverflow 表示输入原始时间戳超出 33 位可表示范围。
	ErrWrapOverflow = errors.New("clock: timestamp exceeds 33-bit range")
	// ErrBackwards 表示去回绕后时间戳发生倒退。
	ErrBackwards = errors.New("clock: non-monotonic timestamp")
	// ErrDuplicate 表示出现重复时间戳（VFR 场景下需要显式修复）。
	ErrDuplicate = errors.New("clock: duplicate timestamp")
	// ErrUndecidable 表示观测点偏离拟合模型过远，不能静默猜测。
	ErrUndecidable = errors.New("clock: undecidable timestamp observation")
	// ErrEmptyInput 表示修复器收到空观测序列。
	ErrEmptyInput = errors.New("clock: empty observation input")
)

// Tick 是统一 90 kHz 时间基准上的一个时间单位。
type Tick int64

// SecondsToTick 把秒（允许小数）换算为 90 kHz Tick，结果四舍五入。
func SecondsToTick(seconds float64) Tick {
	return Tick(math.Round(seconds * float64(TickRate)))
}

// TickToSeconds 把 Tick 换算为秒。
func TickToSeconds(t Tick) float64 {
	return float64(t) / float64(TickRate)
}

// TickToMillis 把 Tick 换算为毫秒，返回浮点以保留亚毫秒精度。
func TickToMillis(t Tick) float64 {
	return float64(t) * 1000.0 / float64(TickRate)
}

// Sample 是一路媒体（音频或视频）的一个样本的时间描述。
//
// RawDTS/RawPTS 是分片内携带的原始 33 位时间戳；DTS/PTS 是去回绕、
// 修复倒退/重复之后落在统一时间轴上的结果。IsUndecidable 为 true 时
// 该样本不允许被计入成功质量统计。
type Sample struct {
	Sequence      int    `json:"sequence"`      // 所属清单片段序号
	TrackID       uint32 `json:"track_id"`      // 轨道标识
	SampleNumber  uint32 `json:"sample_number"` // 片段内样本号（从 1 开始）
	RawDTS        uint64 `json:"raw_dts"`       // 原始 33 位 DTS
	RawPTS        uint64 `json:"raw_pts"`       // 原始 33 位 PTS
	DTS           Tick   `json:"dts"`           // 修复后统一时间轴 DTS
	PTS           Tick   `json:"pts"`           // 修复后统一时间轴 PTS
	IsVideo       bool   `json:"is_video"`
	IsSync        bool   `json:"is_sync"`       // 关键帧 / 随机访问点
	IsUndecidable bool   `json:"is_undecidable"`
	IsDuplicate   bool   `json:"is_duplicate"`  // 修复过的重复时间戳
}

// Gap 描述时间线上的显式空洞：缺失片段或不可判定区段。
type Gap struct {
	Sequence int  `json:"sequence"`
	Start    Tick `json:"start"`
	End      Tick `json:"end"`
	Missing  bool `json:"missing"` // true=缺片；false=存在数据但不可判定
}

// Observation 是漂移拟合的一个输入观测点。
//
// Expected 是按清单标称时长推算的期望时间轴位置，Observed 是设备时钟
// 实际打出的去回绕时间位置，单位都是 90 kHz Tick。
type Observation struct {
	Sequence int  `json:"sequence"`
	Expected Tick `json:"expected"`
	Observed Tick `json:"observed"`
}

// DriftModel 是观测窗口上拟合出的节点线性漂移模型：
// observed ≈ slope*expected + intercept。
type DriftModel struct {
	Slope     float64 `json:"slope"`     // 斜率，1 + driftPPM/1e6
	Intercept float64 `json:"intercept"` // 截距（Tick）
	DriftPPM  float64 `json:"drift_ppm"` // 斜率换算的漂移量
	Window    int     `json:"window"`    // 拟合使用的观测窗口大小
	Points    int     `json:"points"`    // 实际参与拟合的点数
}

// Timeline 是修复后的完整媒体时间线。
type Timeline struct {
	Samples      []Sample     `json:"samples"`
	Gaps         []Gap        `json:"gaps"`
	WrapCount    int          `json:"wrap_count"`
	Drift        DriftModel   `json:"drift"`
	Undecidable  int          `json:"undecidable_count"`
	Duplicate    int          `json:"duplicate_count"`
	Backwards    int          `json:"backwards_count"`
	MinDTS       Tick         `json:"min_dts"`
	MaxDTS       Tick         `json:"max_dts"`
}

// Duration 返回时间线覆盖的 Tick 长度（无样本时为 0）。
func (t *Timeline) Duration() Tick {
	if len(t.Samples) == 0 {
		return 0
	}
	return t.MaxDTS - t.MinDTS
}

// ValidSampleCount 返回可判定（可计入成功质量）样本数。
func (t *Timeline) ValidSampleCount() int {
	return len(t.Samples) - t.Undecidable
}
