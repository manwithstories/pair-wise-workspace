// Package clock 统一媒体时间基准：32 位时间戳回绕展开、滑动窗口漂移估计
// 与可变帧率（VFR）识别。所有对外时间均以整数微秒表示，避免浮点累积误差。
package clock

import (
	"math"

	"github.com/pion/rtp"
)

// Rational 表示有理数时间基准：Num 个时钟滴答对应 Den 秒。
// 典型视频 RTP 时间基准为 90000/1。
type Rational struct {
	Num int64 // 滴答数
	Den int64 // 秒数
}

// Micros 将展开后的滴答数转换为整数微秒。
func (r Rational) Micros(ticks int64) int64 {
	return ticks * 1_000_000 * r.Den / r.Num
}

// 32 位回绕计数器的模数与半程阈值（RFC 3550 §A.1 的 RTP 序列号/时间戳判定）。
const (
	wrapMod  = uint64(1) << 32
	wrapHalf = wrapMod / 2
)

// MaxDriftPPM 限制漂移校正斜率，避免把真实帧率变化误校正为恒定帧率。
const MaxDriftPPM = 1000.0

// Unwrap32 依据上一个已展开值 prev，把当前 32 位回绕计数 current 展开为
// 64 位单调滴答。forward=false 表示这是一次超过半程的反向跳变（设备时钟
// 异常），而非正常回绕；wrapped=true 表示发生了正向 32 位回绕。
func Unwrap32(prev uint64, current uint32) (out uint64, wrapped, backward bool) {
	prevMod := prev & (wrapMod - 1)
	delta := (uint64(current) - prevMod) & (wrapMod - 1)
	switch {
	case delta == 0:
		return prev, false, false
	case delta < wrapHalf:
		// 正向增量：若当前模值小于上一模值，说明跨过了 2^32 边界。
		return prev + delta, uint64(current) < prevMod, false
	default:
		// 大反向跳变：按 RFC 3550 §A.1 解释为向后走。
		return prev + delta - wrapMod, false, true
	}
}

// UnwrapRTP 复用 RTP 时间戳的回绕处理逻辑，展开包内 32 位时间戳。
func UnwrapRTP(prev uint64, pkt *rtp.Packet) (out uint64, wrapped, backward bool) {
	return Unwrap32(prev, pkt.Header.Timestamp)
}

// FrameMark 是一帧在统一时间轴上的标记。
type FrameMark struct {
	Index    int   // 帧序号（从 0 开始）
	Ticks64  int64 // 展开后的 64 位滴答
	Micros   int64 // 统一时间轴上的整数微秒
	Keyframe bool  // 是否为随机访问帧
	Wrapped  bool  // 该帧时间戳触发了 32 位回绕
	Backward bool  // 该帧时间戳为异常反向跳变
}

// 漂移滑动窗口样本数与 VFR 判定阈值。
const (
	driftWindow   = 64
	vfrMinFrac    = 0.25 // 偏离中位时长的帧占比超过 25% 即判 VFR
	vfrRelTol     = 0.05 // 帧间滴答相对偏差超过 5% 视为不同时长
)

// Timeline 维护单条轨道的统一时间轴。
type Timeline struct {
	trackID int
	rate    Rational
	nominal uint64 // 每帧名义滴答数（恒定帧率基准）
	last    uint64
	started bool
	frames  []FrameMark
	window  []driftPoint
	deltas  []uint64 // 窗口内帧间滴答，用于 VFR 判定
	ppm     float64  // 最近一次限幅后的漂移估计（PPM）
	vfr     bool
}

type driftPoint struct {
	x float64 // 帧序号
	y float64 // 展开后滴答
}

// NewTimeline 创建轨道时间轴。nominalStepTicks 为恒定帧率下每帧滴答数。
func NewTimeline(trackID int, rate Rational, nominalStepTicks uint32) *Timeline {
	return &Timeline{trackID: trackID, rate: rate, nominal: uint64(nominalStepTicks)}
}

// TrackID 返回轨道编号。
func (t *Timeline) TrackID() int { return t.trackID }

// Append 输入一帧的原始 32 位时间戳与关键帧标记，返回统一时间轴标记。
func (t *Timeline) Append(raw uint32, keyframe bool) FrameMark {
	idx := len(t.frames)
	var ticks64 int64
	var wrapped, backward bool
	if !t.started {
		t.last = uint64(raw)
		t.started = true
	} else {
		out, w, b := Unwrap32(t.last, raw)
		t.last, wrapped, backward = out, w, b
	}
	ticks64 = int64(t.last)
	mark := FrameMark{
		Index:    idx,
		Ticks64:  ticks64,
		Micros:   t.rate.Micros(ticks64),
		Keyframe: keyframe,
		Wrapped:  wrapped,
		Backward: backward,
	}
	t.frames = append(t.frames, mark)
	t.estimate(idx, ticks64)
	return mark
}

// estimate 用滑动窗口做最小二乘漂移估计，并在确认 VFR 时拒绝平滑校正。
func (t *Timeline) estimate(idx int, ticks64 int64) {
	if idx > 0 {
		t.deltas = append(t.deltas, uint64(ticks64-t.frames[idx-1].Ticks64))
		if len(t.deltas) > driftWindow {
			t.deltas = t.deltas[1:]
		}
	}
	t.window = append(t.window, driftPoint{float64(idx), float64(ticks64)})
	if len(t.window) > driftWindow {
		t.window = t.window[1:]
	}
	if t.vfr || t.isWindowVFR() {
		t.vfr = true
		t.ppm = 0 // 真实 VFR：禁止把它平滑成恒定帧率
		return
	}
	if len(t.window) >= 2 && t.nominal > 0 {
		slope := leastSquaresSlope(t.window)
		ppm := (slope/float64(t.nominal) - 1) * 1_000_000
		t.ppm = clampPPM(ppm)
	}
}

// isWindowVFR 统计窗口内偏离中位帧间滴答的比例，识别真实可变帧率。
func (t *Timeline) isWindowVFR() bool {
	if len(t.deltas) < 8 {
		return false
	}
	med := medianUint64(t.deltas)
	if med == 0 {
		return false
	}
	bad := 0
	for _, d := range t.deltas {
		diff := math.Abs(float64(d)-float64(med)) / float64(med)
		if diff > vfrRelTol {
			bad++
		}
	}
	return float64(bad)/float64(len(t.deltas)) >= vfrMinFrac
}

func leastSquaresSlope(points []driftPoint) float64 {
	var sx, sy, sxx, sxy float64
	for _, p := range points {
		sx += p.x
		sy += p.y
		sxx += p.x * p.x
		sxy += p.x * p.y
	}
	n := float64(len(points))
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

func medianUint64(v []uint64) uint64 {
	cp := append([]uint64(nil), v...)
	// 插入排序即可：窗口最多 64 个样本。
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j-1] > cp[j]; j-- {
			cp[j-1], cp[j] = cp[j], cp[j-1]
		}
	}
	return cp[len(cp)/2]
}

func clampPPM(ppm float64) float64 {
	if ppm > MaxDriftPPM {
		return MaxDriftPPM
	}
	if ppm < -MaxDriftPPM {
		return -MaxDriftPPM
	}
	return ppm
}

// Frames 返回已记录的全部帧标记（副本）。
func (t *Timeline) Frames() []FrameMark {
	out := make([]FrameMark, len(t.frames))
	copy(out, t.frames)
	return out
}

// DriftPPM 返回限幅后的漂移斜率（百万分比）；VFR 轨道返回 0。
func (t *Timeline) DriftPPM() float64 { return t.ppm }

// IsVFR 报告该轨道是否被识别为真实可变帧率。
func (t *Timeline) IsVFR() bool { return t.vfr }

// CorrectMicros 应用限幅后的漂移校正，返回校正后的微秒时间。
func (t *Timeline) CorrectMicros(m FrameMark) int64 {
	if t.ppm == 0 {
		return m.Micros
	}
	correction := 1 - t.ppm/1_000_000
	return int64(float64(m.Micros) * correction)
}
