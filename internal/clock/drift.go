package clock

import (
	"fmt"
	"math/bits"
)

// MaxDriftPPM 是允许的节点时钟漂移上限（百万分之一）。
// 超过该值的估计会被限幅，避免单个异常 PCR 把整条时间线拉偏。
const MaxDriftPPM = 50_000 // 5%，演示夹具用的宽松硬上限

// DriftEstimator 根据 (本地接收刻度, 对端 PCR 刻度) 采样对，
// 用最小二乘斜率估计节点间时钟漂移，并对结果限幅。
type DriftEstimator struct {
	sumX, sumY, sumXY, sumXX Tick
	n                        int
	firstX, firstY           Tick
	lastX, lastY             Tick
	ppm                      float64
	limited                  bool
}

// Sample 输入一组采样：local 是本节点 27 MHz 接收时刻，
// remote 是报文 PCR 展开后的对端时刻。
func (d *DriftEstimator) Sample(local, remote Tick) {
	if d.n == 0 {
		d.firstX, d.firstY = local, remote
	}
	d.lastX, d.lastY = local, remote
	x := local - d.firstX
	y := remote - d.firstY
	d.sumX += x
	d.sumY += y
	d.sumXY += x * y
	d.sumXX += x * x
	d.n++
	d.recompute()
}

func (d *DriftEstimator) recompute() {
	if d.n < 2 {
		d.ppm, d.limited = 0, false
		return
	}
	// 最小二乘斜率 slope = Sxy / Sxx，其中
	// Sxx = Σx² - (Σx)²/n，Sxy = Σxy - ΣxΣy/n。
	n := Tick(d.n)
	sxx := d.sumXX - d.sumX*d.sumX/n
	sxy := d.sumXY - d.sumX*d.sumY/n
	if sxx == 0 {
		d.ppm, d.limited = 0, false
		return
	}
	// ppm = (Sxy/Sxx - 1) * 1e6 = (Sxy-Sxx)*1e6/Sxx。
	// 差值可能为负，先取符号；绝对值做 128 位除法避免 int64 溢出。
	delta := sxy - sxx
	sign := Tick(1)
	if delta < 0 {
		sign, delta = -1, -delta
	}
	hi, lo := bits.Mul64(uint64(delta), 1_000_000)
	quo, _ := bits.Div64(hi, lo, uint64(sxx))
	ppmX1000 := sign * Tick(quo) * 1000
	ppm := float64(ppmX1000) / 1000.0
	d.limited = false
	if ppm > MaxDriftPPM {
		ppm, d.limited = MaxDriftPPM, true
	} else if ppm < -MaxDriftPPM {
		ppm, d.limited = -MaxDriftPPM, true
	}
	d.ppm = ppm
}

// PPM 返回限幅后的漂移估计（百万分之一，正号表示远端时钟偏快）。
func (d *DriftEstimator) PPM() float64 { return d.ppm }

// Limited 返回最后一次重算是否触发了限幅。
func (d *DriftEstimator) Limited() bool { return d.limited }

// Samples 返回已输入的采样数。
func (d *DriftEstimator) Samples() int { return d.n }

// String 便于终端诊断输出。
func (d *DriftEstimator) String() string {
	tag := ""
	if d.limited {
		tag = " (clamped)"
	}
	return fmt.Sprintf("drift=%.2fppm%s n=%d", d.ppm, tag, d.n)
}
