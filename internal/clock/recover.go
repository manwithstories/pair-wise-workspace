// Package clock 融合 RTCP Sender Report 恢复跨节点统一媒体时钟。
// 职责：32 位 RTP 时间戳回绕扩展、基于 SR 锚点的漂移估计（钳制 ±200ppm）、
// 以及把可变帧率（15..60fps）的视频时间标记校准到同一纳秒时间轴。
package clock

import (
	"relay/internal/media"
)

// SRPoint 是一条 RTCP Sender Report 提供的时钟对应点。
type SRPoint struct {
	NTPSeconds  uint32
	NTPFraction uint32
	RTP         uint32 // SR 携带的 32 位 RTP 时间戳
}

// NTPNanos 把 NTP 秒+小数部分换算为 Unix 纪元纳秒。
func (s SRPoint) NTPNanos() int64 {
	const ntpEpochOffset = 2208988800 // 1900-01-01 与 1970-01-01 的秒差
	sec := int64(s.NTPSeconds) - ntpEpochOffset
	// NTP 小数是 32 位定点数（单位 1/2^32 秒），换算为纳秒。
	frac := int64(s.NTPFraction) * media.RateNanosec >> 32
	return sec*media.RateNanosec + frac
}

// Recover 是单 SSRC 的时钟恢复器，串行处理同一流的数据报。
// 输出统一为纳秒时间轴上的 media.MediaTime，并对外暴露当前漂移估计。
type Recover struct {
	rate      int64 // RTP 时钟基率（音频如 48000，视频 90000）
	baseExt   int64 // 首个包扩展后的 RTP tick
	baseNanos int64 // 首个包对应的纳秒（来自首个 SR 或本地零点）
	lastExt   int64
	started   bool
	anchored  bool // 是否已收到至少一个 SR 锚点

	// SR 锚点对（扩展 RTP tick, NTP 纳秒），用于线性漂移估计。
	anchors []anchor

	// SR 专用的只增扩展参考，与数据包 lastExt 解耦以抵御乱序。
	srStarted bool
	srLastExt int64

	ppm       int    // 当前采用的漂移（ppm），钳制在 ±MaxPPM
	wrapCount uint64 // 观测到的 32 位回绕次数（诊断用）
}

type anchor struct {
	rtp   int64
	ntpNs int64
}

// maxAnchors 限制参与回归的锚点数，保证内存有界。
const maxAnchors = 64

// NewRecover 创建时钟恢复器，rate 为该流的 RTP 时钟基率。
func NewRecover(rate int) *Recover {
	if rate <= 0 {
		rate = media.RateAudio48k
	}
	return &Recover{rate: int64(rate)}
}

// PPM 返回当前漂移估计（百万分率）。
func (r *Recover) PPM() int { return r.ppm }

// WrapCount 返回观测到的时间戳回绕次数。
func (r *Recover) WrapCount() uint64 { return r.wrapCount }

// ObserveRTP 处理一个 RTP 包的 32 位时间戳，返回扩展后的 64 位 tick。
// 乱序到达也能正确扩展；检测到按序向前跨越 2^32 边界时累加回绕计数。
func (r *Recover) ObserveRTP(ts uint32) int64 {
	if !r.started {
		r.started = true
		r.lastExt = int64(ts)
		r.baseExt = r.lastExt
		return r.lastExt
	}
	prev := r.lastExt
	next := media.Extend32(prev, ts)
	// 按序（更新）的包推进参考点；跨圈即记一次回绕。乱序包 next<=prev，不计数。
	if next > prev {
		if prev>>32 != next>>32 {
			r.wrapCount += uint64(next>>32 - prev>>32)
		}
		r.lastExt = next
	}
	return next
}

// ObserveSR 融合一条 Sender Report，更新漂移估计与纳秒锚点。
// SR 序号使用独立的只增扩展参考，不受数据包乱序导致的 lastExt 回退影响。
func (r *Recover) ObserveSR(sr SRPoint) {
	if !r.srStarted {
		r.srStarted = true
		r.srLastExt = int64(sr.RTP)
	}
	ext := media.Extend32(r.srLastExt, sr.RTP)
	if ext > r.srLastExt {
		r.srLastExt = ext
	}
	ntp := sr.NTPNanos()
	r.anchors = append(r.anchors, anchor{rtp: ext, ntpNs: ntp})
	if len(r.anchors) > maxAnchors {
		// 丢弃最旧的一半，保留近期漂移特征。
		r.anchors = append([]anchor(nil), r.anchors[len(r.anchors)/2:]...)
	}
	if !r.anchored {
		r.anchored = true
		r.baseExt = ext
		r.baseNanos = ntp
	}
	r.ppm = r.estimatePPM()
}

// estimatePPM 用首尾锚点做最小二乘斜率估计并钳制到 ±200ppm。
// 两点等间隔时闭式解与普通斜率一致。
func (r *Recover) estimatePPM() int {
	n := len(r.anchors)
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumXX float64
	for _, a := range r.anchors {
		// x: RTP 经过标称基率换算的纳秒；y: NTP 纳秒。
		x := float64(a.rtp) * float64(media.RateNanosec) / float64(r.rate)
		y := float64(a.ntpNs)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	fn := float64(n)
	den := fn*sumXX - sumX*sumX
	if den == 0 {
		return 0
	}
	slope := (fn*sumXY - sumX*sumY) / den // NTP 纳秒 / RTP 标称纳秒
	ppm := int((1 - slope) * 1_000_000)
	if ppm > media.MaxPPM {
		ppm = media.MaxPPM
	} else if ppm < -media.MaxPPM {
		ppm = -media.MaxPPM
	}
	return ppm
}

// ToNanos 把扩展 RTP tick 映射到统一纳秒时间轴，并施加漂移修正。
// 首个 SR 之前使用本地零点（baseNanos=0），锚点到达后以 SR 对齐。
func (r *Recover) ToNanos(extTicks int64) media.MediaTime {
	rel := extTicks - r.baseExt
	corrected := media.ScalePPM(rel, r.ppm)
	ns := r.baseNanos + corrected*media.RateNanosec/r.rate
	return media.New(ns, media.RateNanosec)
}

// Anchored 报告是否已与 RTCP 墙钟对齐。
func (r *Recover) Anchored() bool { return r.anchored }

// 可变帧率合法边界（fps）。
const (
	VFRMinFPS = 15
	VFRMaxFPS = 60
)

// VFRResult 是单个视频时间标记的校准结果。
type VFRResult struct {
	Nanos      int64 // 统一时间轴上的呈现时刻
	IntervalNs int64 // 与上一帧的间隔；首帧为 0
	FPS        float64
	InRange    bool // 间隔是否落在 15..60fps 对应区间
}

// VFRTracker 校准可变帧率视频标记。视频 RTP 时间戳基率固定 90kHz，
// 每帧的标记间隔允许在 15..60fps 之间变化，但全部映射到纳秒时间轴。
type VFRTracker struct {
	rec       *Recover
	lastNanos int64
	haveLast  bool
}

// NewVFRTracker 以 90kHz 视频时钟基率构造校准器。
func NewVFRTracker() *VFRTracker {
	return &VFRTracker{rec: NewRecover(media.RateVideo90k)}
}

// Recover 暴露内部恢复器，便于注入同一流的 RTCP SR。
func (v *VFRTracker) Recover() *Recover { return v.rec }

// Observe 处理一个视频帧的 32 位 RTP 标记并校准到统一时间轴。
func (v *VFRTracker) Observe(marker uint32) VFRResult {
	ext := v.rec.ObserveRTP(marker)
	mt := v.rec.ToNanos(ext)
	ns := mt.Ticks
	res := VFRResult{Nanos: ns}
	if v.haveLast {
		res.IntervalNs = ns - v.lastNanos
		if res.IntervalNs > 0 {
			res.FPS = float64(media.RateNanosec) / float64(res.IntervalNs)
			// 90kHz 量化带来约 0.002fps 抖动，留 1% 容差。
			res.InRange = res.FPS >= VFRMinFPS*0.99 && res.FPS <= VFRMaxFPS*1.01
		}
	}
	v.lastNanos = ns
	v.haveLast = true
	return res
}

// ObserveSR 透传 Sender Report 给底层恢复器。
func (v *VFRTracker) ObserveSR(sr SRPoint) { v.rec.ObserveSR(sr) }
