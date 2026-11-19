package rtp

import "sort"

// bwSample 是一次到达观测：纳秒时间戳与字节数。
type bwSample struct {
	t int64
	b int
}

// bwEstimator 用滑动时间窗口估算实时带宽（bit/s），
// 并保留首窗口的稳态值作为等待预算缩放的参考基准。
type bwEstimator struct {
	windowNano int64
	samples    []bwSample
	bytes      int64
	reference  float64
	hasRef     bool
}

const defaultBWWindowNano = int64(200_000_000) // 200ms 滑动窗口

func newBWEstimator() *bwEstimator {
	return &bwEstimator{windowNano: defaultBWWindowNano}
}

// observe 记录一个到达包。
func (b *bwEstimator) observe(arrivalNano int64, size int) {
	b.samples = append(b.samples, bwSample{arrivalNano, size})
	b.bytes += int64(size)
	b.evict(arrivalNano)
}

func (b *bwEstimator) evict(now int64) {
	cutoff := now - b.windowNano
	drop := 0
	for drop < len(b.samples) && b.samples[drop].t < cutoff {
		b.bytes -= int64(b.samples[drop].b)
		drop++
	}
	if drop > 0 {
		b.samples = append(b.samples[:0], b.samples[drop:]...)
	}
}

// bandwidth 返回当前窗口带宽（bit/s）；样本不足时回退为 0。
func (b *bwEstimator) bandwidth() float64 {
	if len(b.samples) < 2 {
		return 0
	}
	sort.Slice(b.samples, func(i, j int) bool { return b.samples[i].t < b.samples[j].t })
	dur := b.samples[len(b.samples)-1].t - b.samples[0].t
	if dur <= 0 {
		return 0
	}
	bps := float64(b.bytes*8) / (float64(dur) / 1e9)
	// 首个完整窗口的中位带宽固化为参考基准，避免自引用缩放。
	if !b.hasRef && dur >= b.windowNano/2 {
		b.reference = bps
		b.hasRef = true
	}
	return bps
}

// referenceBps 返回参考带宽；尚未建立时返回当前值（缩放比退化为 1）。
func (b *bwEstimator) referenceBps() float64 {
	if b.hasRef && b.reference > 0 {
		return b.reference
	}
	return b.bandwidth()
}
