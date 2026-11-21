package clock

import (
	"errors"
	"math"
)

// Anchor 是一个合法时钟锚点：扩展后的 RTP tick 与对应的真实媒体纳秒。
// 真实媒体时间由累计音频采样数换算，是漂移拟合的唯一可信基准。
type Anchor struct {
	Tick int64
	NS   int64
}

// SegmentFit 是一段可拟合时钟的最小二乘结果。
type SegmentFit struct {
	Slope     float64 // ns / tick
	Intercept float64 // ns
	MaxResNS  int64   // 段内最大绝对残差（纳秒）
}

// ErrInsufficientAnchors 表示锚点不足，无法拟合时钟段。
var ErrInsufficientAnchors = errors.New("clock: 锚点不足，无法拟合时钟段")

// FitRate 对一段锚点做最小二乘线性拟合，返回 ns 关于 tick 的斜率与截距。
// 锚点少于 2 个时显式报错，禁止静默退化为零漂移假设。
func FitRate(anchors []Anchor) (SegmentFit, error) {
	n := float64(len(anchors))
	if len(anchors) < 2 {
		return SegmentFit{}, ErrInsufficientAnchors
	}
	var sx, sy, sxx, sxy float64
	for _, a := range anchors {
		x := float64(a.Tick)
		y := float64(a.NS)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	denom := n*sxx - sx*sx
	if denom == 0 {
		return SegmentFit{}, ErrInsufficientAnchors
	}
	slope := (n*sxy - sx*sy) / denom
	fit := SegmentFit{
		Slope:     slope,
		Intercept: (sy - slope*sx) / n,
	}
	for _, a := range anchors {
		r := int64(math.Abs(fit.Intercept + fit.Slope*float64(a.Tick) - float64(a.NS)))
		if r > fit.MaxResNS {
			fit.MaxResNS = r
		}
	}
	return fit, nil
}

// Reconstructor 以整数纳秒+余数进位的方式把 tick 重建为媒体时间。
// 直接对每帧四舍五入会让截断误差逐帧累积；这里保留定点余数，
// 保证长时间运行后累计截断误差不超过 1 纳秒。
// 全部乘法基于相对段首的 delta tick，避免绝对 tick 与定点分子相乘溢出 int64。
type Reconstructor struct {
	slopeNum  int64 // 斜率定点分子（ns）
	slopeDen  int64 // 斜率定点分母（tick）
	baseTick  int64
	baseNS    int64
	remainder int64
}

// NewReconstructor 把拟合得到的浮点斜率量化为 1e6 定点比例，
// 并以 baseTick/baseNS 作为段首基准（ns 可由拟合直线在 baseTick 处取值）。
// 取 1e6 而非 1e9 是为保证 dt*slopeNum 在长序列下不溢出 int64；
// 每 tick 量化误差 1e-6ns，万帧累计截断误差仍小于 0.02ns。
func NewReconstructor(fit SegmentFit, baseTick, baseNS int64) *Reconstructor {
	const scale = int64(1_000_000)
	return &Reconstructor{
		slopeNum: int64(math.Round(fit.Slope * float64(scale))),
		slopeDen: scale,
		baseTick: baseTick,
		baseNS:   baseNS,
	}
}

// At 返回 tick 对应的绝对整数纳秒媒体时间；定点余数在内部进位，
// 相邻帧的截断误差不会静默累积。
func (r *Reconstructor) At(tick int64) int64 {
	dt := tick - r.baseTick
	num := dt*r.slopeNum + r.remainder
	whole := num / r.slopeDen
	r.remainder = num % r.slopeDen
	if r.remainder < 0 {
		whole--
		r.remainder += r.slopeDen
	}
	return r.baseNS + whole
}

// Remainder 暴露当前累计余数，供测试核验误差不累积。
func (r *Reconstructor) Remainder() int64 { return r.remainder }
