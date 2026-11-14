package rc

import (
	"fmt"
	"math"

	"rcx/internal/codec"
	"rcx/internal/fixture"
)

// 分片模式：恒码率或带上限。
const (
	ModeCBR    = "cbr"
	ModeCapped = "capped"
)

// SegmentPlan 是一段的分片计划。
type SegmentPlan struct {
	Index      int   `json:"index"`
	StartFrame int   `json:"start_frame"`
	Frames     int   `json:"frames"`
	TargetBits int64 `json:"target_bits"`
}

// Plan 是一次编码任务的分片规划。
type Plan struct {
	Input         string        `json:"input"`
	Width         int           `json:"width"`
	Height        int           `json:"height"`
	FPS           int           `json:"fps"`
	SegmentFrames int           `json:"segment_frames"`
	Frames        int           `json:"frames"`
	BitrateBps    int64         `json:"bitrate_bps"`
	Mode          string        `json:"mode"`
	Segments      []SegmentPlan `json:"segments"`
}

// BuildPlan 按帧率与段时长切段，目标比特计入缓冲结余（小数进位逐段结转）。
func BuildPlan(input string, w, h, fps, segSec, frames int, bitrateBps int64, mode string) (*Plan, error) {
	if fps <= 0 || segSec <= 0 {
		return nil, fmt.Errorf("fps 与段时长须为正，当前 fps=%d segment=%d", fps, segSec)
	}
	if bitrateBps <= 0 {
		return nil, fmt.Errorf("码率须为正，当前 %d", bitrateBps)
	}
	if mode != ModeCBR && mode != ModeCapped {
		return nil, fmt.Errorf("未知模式 %q（可选 cbr|capped）", mode)
	}
	if frames <= 0 {
		return nil, fmt.Errorf("帧数须为正，当前 %d", frames)
	}
	spf := fps * segSec
	p := &Plan{
		Input: input, Width: w, Height: h, FPS: fps,
		SegmentFrames: spf, Frames: frames,
		BitrateBps: bitrateBps, Mode: mode,
	}
	carry := 0.0
	for start := 0; start < frames; start += spf {
		n := spf
		if start+n > frames {
			n = frames - start
		}
		exact := float64(bitrateBps) * float64(n) / float64(fps)
		target := math.Floor(exact + carry)
		carry += exact - target
		p.Segments = append(p.Segments, SegmentPlan{
			Index: len(p.Segments), StartFrame: start, Frames: n, TargetBits: int64(target),
		})
	}
	return p, nil
}

// DeviationPct 计算实际与目标的偏差百分比（绝对值）。
func DeviationPct(actual, target int64) float64 {
	if target <= 0 {
		return 0
	}
	return math.Abs(float64(actual-target)) / float64(target) * 100
}

// VBVCheck 模拟段内缓冲区：初始半满，逐帧注入比特、按码率匀速排出。
// 返回最小水位占比与是否全程未下溢未溢出。
func VBVCheck(frameBits []int64, bitrateBps int64, fps int) (minRatio float64, ok bool) {
	capacity := float64(bitrateBps) // 1 秒码率作为缓冲容量
	drain := float64(bitrateBps) / float64(fps)
	level := capacity / 2
	minLevel := level
	ok = true
	for _, b := range frameBits {
		level += float64(b) - drain
		if level < 0 || level > capacity {
			ok = false
		}
		if level < minLevel {
			minLevel = level
		}
	}
	if minLevel < 0 {
		minLevel = 0
	}
	return minLevel / capacity, ok
}

// Controller 执行两遍码率控制。
type Controller struct {
	SampleQPs   []float64 // 第一遍采样 QP
	MaxFallback int       // 超预算时抬 QP 重编的最大次数
	Tolerance   float64   // 偏差容忍（如 3.0 表示 3%）
}

// NewController 返回默认控制器。
func NewController() *Controller {
	return &Controller{SampleQPs: []float64{4, 14, 28, 48}, MaxFallback: 2, Tolerance: 3.0}
}

// snapQP 把 QP 对齐到 0.125 步进并夹取。
// 步进需足够细：QP 附近码率变化率约为 B/Q，0.125 步进对应的
// 码率变化通常小于 3% 容忍窗口，保证回退能落入达标区间。
func snapQP(qp float64) float64 {
	return codec.ClampQP(math.Round(qp*8) / 8)
}

// firstPass 采样段内帧，拟合 R = A·Q^-B。
func (c *Controller) firstPass(frames []*fixture.Frame, seg SegmentPlan) RateModel {
	n := seg.Frames
	idxs := []int{0, n / 2, n - 1}
	seen := map[int]bool{}
	samples := []*fixture.Frame{}
	for _, i := range idxs {
		if !seen[i] {
			seen[i] = true
			samples = append(samples, frames[seg.StartFrame+i])
		}
	}
	scale := float64(n) / float64(len(samples))
	qs := make([]float64, 0, len(c.SampleQPs))
	rs := make([]float64, 0, len(c.SampleQPs))
	for _, qp := range c.SampleQPs {
		var bits int64
		for _, f := range samples {
			bits += codec.FrameBits(f, qp)
		}
		qs = append(qs, qp)
		rs = append(rs, float64(bits)*scale)
	}
	return Fit(qs, rs)
}

// EncodeSegment 两遍编码一段：第一遍拟合模型反解 QP，第二遍实编，
// 偏差超容忍或 VBV 违例时抬/降 QP 重编，最多回退 MaxFallback 次。
// 返回结果、实际 QP、是否达标。
func (c *Controller) EncodeSegment(frames []*fixture.Frame, seg SegmentPlan, bitrateBps int64, fps int) (*codec.SegResult, float64, float64, bool) {
	model := c.firstPass(frames, seg)
	qp := snapQP(model.SolveQP(float64(seg.TargetBits)))
	var best *codec.SegResult
	bestQP := qp
	bestDev := math.Inf(1)
	vbvMin := 0.0
	histQ := []float64{}
	histR := []float64{}
	for attempt := 0; attempt <= c.MaxFallback; attempt++ {
		res := codec.EncodeSegment(frames[seg.StartFrame:seg.StartFrame+seg.Frames], qp, false)
		dev := DeviationPct(res.Bits, seg.TargetBits)
		ratio, vbvOK := VBVCheck(res.FrameBits, bitrateBps, fps)
		if dev < bestDev {
			best, bestQP, bestDev, vbvMin = res, qp, dev, ratio
		}
		if dev <= c.Tolerance && vbvOK {
			return res, qp, ratio, true
		}
		// 局部斜率：有两个实测点时用实测斜率，否则用第一遍模型斜率。
		histQ = append(histQ, qp)
		histR = append(histR, float64(res.Bits))
		b := model.B
		if n := len(histQ); n >= 2 {
			if lb := math.Log(histR[n-2]/histR[n-1]) / math.Log(histQ[n-1]/histQ[n-2]); lb > 0.05 {
				b = lb
			}
		}
		// 按斜率外推新 QP：actual>target 抬 QP，反之降 QP。
		next := snapQP(qp * math.Pow(float64(res.Bits)/float64(seg.TargetBits), 1/b))
		if next == qp {
			if res.Bits > seg.TargetBits && qp < codec.QPMax {
				next = snapQP(qp + 0.25)
			} else if res.Bits < seg.TargetBits && qp > codec.QPMin {
				next = snapQP(qp - 0.25)
			} else {
				break
			}
			if next == qp {
				break
			}
		}
		qp = next
	}
	return best, bestQP, vbvMin, false
}
