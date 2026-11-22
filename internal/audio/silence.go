// Package audio 在轨道采样窗口上计算 RMS 并检测静音区间。
//
// 窗口可以跨越多个片段边界拼接计算；当窗口末尾的样本不完整（截断）
// 时，窗口标记为 incomplete 而不是静音，避免把数据缺失误报为完整静音。
package audio

import (
	"math"

	"mediad/internal/model"
)

// SilenceThreshold 是判定静音的 RMS 上限（线性 PCM 幅值量纲，0..1）。
const SilenceThreshold = 0.01

// Window 是一次 RMS 计算的结果。
type Window struct {
	TrackID    string
	StartTick  int64
	EndTick    int64
	Samples    int
	RMS        float64
	Silent     bool
	Incomplete bool // 窗口含截断样本，RMS 不代表完整窗口
}

// PCM16 把小端 16-bit PCM 字节解码为 [-1,1] 浮点样本。
// 字节数为奇数时最后一个字节忽略，并视为不完整输入。
func PCM16(data []byte) (samples []float64, odd bool) {
	n := len(data) / 2
	samples = make([]float64, n)
	for i := 0; i < n; i++ {
		v := int16(uint16(data[2*i]) | uint16(data[2*i+1])<<8)
		samples[i] = float64(v) / 32768.0
	}
	return samples, len(data)%2 == 1
}

// RMS 计算一组 PCM 样本的均方根。
func RMS(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(samples)))
}

// SegmentPCM 汇总片段在指定轨道上的可解析 PCM 字节与完整性。
type SegmentPCM struct {
	SegmentID string
	Data      []byte
	Complete  bool
}

// DetectWindows 在拼接的片段 PCM 流上按固定样本数开窗检测静音。
//
// 每 windowSamples 个 PCM 样本形成一个窗口；跨片段边界无缝拼接。
// 若窗口覆盖到任一不完整片段，窗口 Incomplete=true 且 Silent=false。
func DetectWindows(trackID string, startTick int64, tickPerSample int64,
	windowSamples int, segs []SegmentPCM) []Window {
	if windowSamples <= 0 || tickPerSample <= 0 {
		return nil
	}
	var windows []Window
	buf := make([]float64, 0, windowSamples)
	windowComplete := true
	windowStart := startTick
	consumed := 0 // 当前窗口已累积样本数，用于 tick 边界推进

	flush := func(force bool) {
		if len(buf) == 0 {
			return
		}
		if !force && len(buf) < windowSamples {
			return
		}
		w := Window{
			TrackID:    trackID,
			StartTick:  windowStart,
			EndTick:    windowStart + int64(len(buf))*tickPerSample,
			Samples:    len(buf),
			RMS:        RMS(buf),
			Incomplete: !windowComplete,
		}
		// 不完整窗口绝不判静音；完整窗口按阈值判定。
		w.Silent = !w.Incomplete && w.RMS <= SilenceThreshold
		windows = append(windows, w)
		buf = buf[:0]
		windowComplete = true
		windowStart = w.EndTick
		consumed = 0
	}

	for _, seg := range segs {
		pcm, odd := PCM16(seg.Data)
		complete := seg.Complete && !odd
		for _, v := range pcm {
			buf = append(buf, v)
			consumed++
			if len(buf) >= windowSamples {
				if !complete {
					windowComplete = false
				}
				flush(true)
			}
		}
		if !complete {
			// 不完整片段的任何残余样本都会污染当前窗口。
			windowComplete = false
		}
	}
	flush(true) // 尾部不足一个窗口的残余也输出，但标注样本数。
	return windows
}

// SilenceRanges 从窗口序列中合并连续静音窗口，返回 tick 区间（闭开）。
// 不完整窗口会中断静音区间，防止把截断误判为长静音。
func SilenceRanges(ws []Window) [][2]int64 {
	var out [][2]int64
	var start int64
	in := false
	for _, w := range ws {
		if w.Silent && !w.Incomplete {
			if !in {
				start = w.StartTick
				in = true
			}
			continue
		}
		if in {
			out = append(out, [2]int64{start, w.StartTick})
			in = false
		}
	}
	if in {
		last := ws[len(ws)-1]
		out = append(out, [2]int64{start, last.EndTick})
	}
	return out
}

// SegmentComplete 是供流水线适配的辅助：从模型片段状态推断完整性。
func SegmentComplete(s model.Segment) bool { return s.Complete() }
