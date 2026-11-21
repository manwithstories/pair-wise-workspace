// Package audio 实现重采样与声道矩阵变换，输出统一规格的 PCM。
package audio

import (
	"mediadeliver/internal/fault"
)

// PCM 是交错采样的浮点音频。
type PCM struct {
	SampleRate int64     `json:"sample_rate"`
	Channels   int       `json:"channels"`
	Frames     []float64 `json:"-"` // 交错采样，长度 = 帧数 * Channels
}

// NumFrames 返回完整采样帧数。
func (p PCM) NumFrames() int {
	if p.Channels <= 0 {
		return 0
	}
	return len(p.Frames) / p.Channels
}

// Convert 将输入统一转换到目标采样率与声道布局。
// 短尾不足一个采样帧时记边界错误（可恢复）并保留前段结果。
func Convert(in PCM, targetRate int64, targetCh int) (PCM, fault.List) {
	var faults fault.List
	// 边界检查：截断不完整帧
	if rem := len(in.Frames) % in.Channels; rem != 0 {
		keep := len(in.Frames) - rem
		faults = faults.Add(fault.Fault{Stage: fault.StageConvert, Source: "pcm",
			Offset: int64(keep), Recoverable: true,
			Message: "trailing samples below one frame, dropped",
			Advice:  "保留前段结果，检查采集缓冲对齐"})
		in.Frames = in.Frames[:keep]
	}
	out := PCM{SampleRate: targetRate, Channels: targetCh}
	nIn := in.NumFrames()
	if nIn == 0 {
		return out, faults
	}
	// 重采样（线性插值）：输出帧数按统一时间基准换算
	nOut := int((int64(nIn)*targetRate + in.SampleRate/2) / in.SampleRate)
	mixed := make([]float64, nOut*targetCh)
	for i := 0; i < nOut; i++ {
		// 输入位置（源采样单位）
		posNum := int64(i) * in.SampleRate
		idx := posNum / targetRate
		frac := float64(posNum%targetRate) / float64(targetRate)
		if idx >= int64(nIn)-1 {
			idx = int64(nIn) - 1
			frac = 0
		}
		var next int64 = idx
		if idx+1 < int64(nIn) {
			next = idx + 1
		}
		for c := 0; c < targetCh; c++ {
			a := channelAt(in, int(idx), c)
			b := channelAt(in, int(next), c)
			mixed[i*targetCh+c] = a + (b-a)*frac
		}
	}
	out.Frames = mixed
	return out, faults
}

// channelAt 按声道矩阵取输入声道在输出声道 c 上的贡献源。
// 单声道复制到所有输出；多声道按下混矩阵（奇偶分组平均）。
func channelAt(in PCM, frame, outCh int) float64 {
	base := frame * in.Channels
	switch {
	case in.Channels == 1:
		return in.Frames[base]
	case in.Channels == 2:
		if outCh < 2 {
			return in.Frames[base+outCh]
		}
		return 0.5 * (in.Frames[base] + in.Frames[base+1])
	default:
		// 多声道下混：输出 L 取偶数声道平均，R 取奇数声道平均
		sum, n := 0.0, 0
		for ch := outCh % 2; ch < in.Channels; ch += 2 {
			sum += in.Frames[base+ch]
			n++
		}
		return sum / float64(n)
	}
}
