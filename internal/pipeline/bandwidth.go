package pipeline

import "time"

// BandwidthEstimator 在可配置滑动窗口内估算接收带宽（Mbps），
// 突降时建议缩小接收窗口；它只提供建议，绝不丢弃已排队块。
type BandwidthEstimator struct {
	window      time.Duration
	samples     []bwSample
	baseWindow  int // 正常接收窗口（包数）
	minWindow   int // 突降时允许的最小窗口
	lastMbps    float64
	hasSample   bool
	// 突降判定阈值：估算值低于峰值的该比例即视为突降。
	dropRatio float64
}

type bwSample struct {
	at    time.Time
	bytes int
}

// NewBandwidthEstimator 构造滑动窗口带宽估算器。
// window 为统计窗口；baseWindow/minWindow 为接收窗口建议的上下界。
func NewBandwidthEstimator(window time.Duration, baseWindow, minWindow int) *BandwidthEstimator {
	if window <= 0 {
		window = time.Second
	}
	if minWindow <= 0 || minWindow > baseWindow {
		minWindow = baseWindow
	}
	return &BandwidthEstimator{
		window:     window,
		baseWindow: baseWindow,
		minWindow:  minWindow,
		dropRatio:  0.5,
	}
}

// Observe 记录一次接收：now 时刻收到 n 字节。
func (b *BandwidthEstimator) Observe(now time.Time, n int) {
	b.samples = append(b.samples, bwSample{at: now, bytes: n})
	b.evict(now)
	var total int
	for _, s := range b.samples {
		total += s.bytes
	}
	secs := b.window.Seconds()
	if len(b.samples) > 0 {
		if span := now.Sub(b.samples[0].at).Seconds(); span > secs {
			secs = span
		}
	}
	if secs <= 0 {
		secs = b.window.Seconds()
	}
	b.lastMbps = float64(total*8) / (secs * 1_000_000)
	b.hasSample = true
}

func (b *BandwidthEstimator) evict(now time.Time) {
	cutoff := now.Add(-b.window)
	keep := b.samples[:0]
	for _, s := range b.samples {
		if !s.at.Before(cutoff) {
			keep = append(keep, s)
		}
	}
	b.samples = keep
}

// Mbps 返回最近一次估算的带宽（Mbps）。
func (b *BandwidthEstimator) Mbps() float64 { return b.lastMbps }

// HasSample 表示是否已有观测。
func (b *BandwidthEstimator) HasSample() bool { return b.hasSample }

// SuggestWindow 根据当前带宽相对配置上限 targetMbps 的占用比例，
// 在 [minWindow, baseWindow] 内给出接收窗口建议。突降时缩小，
// 恢复时放大；调用方据此调整窗口，但队列中已有的块不受影响。
func (b *BandwidthEstimator) SuggestWindow(targetMbps float64) int {
	if !b.hasSample || targetMbps <= 0 {
		return b.baseWindow
	}
	ratio := b.lastMbps / targetMbps
	if ratio >= 1 {
		return b.baseWindow
	}
	if ratio < b.dropRatio {
		return b.minWindow // 突降：收紧到最小窗口
	}
	// 线性插值。
	span := b.baseWindow - b.minWindow
	scaled := b.minWindow + int(float64(span)*ratio)
	if scaled < b.minWindow {
		scaled = b.minWindow
	}
	if scaled > b.baseWindow {
		scaled = b.baseWindow
	}
	return scaled
}
