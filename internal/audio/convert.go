// Package audio 执行采样率转换、声道归一与响度处理。
// 所有时间定位基于采样时间而非包到达时间。
package audio

import (
	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
)

// supportedRates 支持的采样率集合。
var supportedRates = map[int]bool{
	8000: true, 16000: true, 22050: true, 24000: true,
	32000: true, 44100: true, 48000: true, 96000: true,
}

// CheckRate 校验采样率，不支持时返回可定位错误。
func CheckRate(rate int) error {
	if !supportedRates[rate] {
		return fault.New(fault.KindAudio, fault.Fail, "audio", 0, -1,
			"unsupported sample rate %d", rate)
	}
	return nil
}

// NormalizeChannels 把交错采样从 from 声道归一到 to 声道。
// 支持 1↔2 与同声道直通；其他不匹配返回可定位错误。
func NormalizeChannels(samples []float64, from, to int) ([]float64, error) {
	if from <= 0 || to <= 0 {
		return nil, fault.New(fault.KindAudio, fault.Fail, "audio", 0, -1,
			"invalid channel count from=%d to=%d", from, to)
	}
	if len(samples)%from != 0 {
		return nil, fault.New(fault.KindAudio, fault.Fail, "audio", 0, -1,
			"non-integral tail block: %d samples not multiple of %d channels", len(samples), from)
	}
	if from == to {
		return samples, nil
	}
	frames := len(samples) / from
	out := make([]float64, frames*to)
	switch {
	case from == 1 && to == 2:
		for i := 0; i < frames; i++ {
			out[2*i], out[2*i+1] = samples[i], samples[i]
		}
	case from == 2 && to == 1:
		for i := 0; i < frames; i++ {
			out[i] = (samples[2*i] + samples[2*i+1]) / 2
		}
	default:
		return nil, fault.New(fault.KindAudio, fault.Fail, "audio", 0, -1,
			"channel layout mismatch: cannot map %d to %d channels", from, to)
	}
	return out, nil
}

// Resampler 流式重采样器：按采样时间（而非到达时间）做线性插值，
// 跨块保留边界帧，限制单块内存占用。
type Resampler struct {
	InRate, OutRate, Channels int
	outIdx                    int64 // 下一个输出采样序号（每声道）
	prev                      []float64
	havePrev                  bool
	inBase                    int64 // 当前块首帧的全局输入序号
}

// NewResampler 创建重采样器；采样率不支持时返回错误。
func NewResampler(inRate, outRate, channels int) (*Resampler, error) {
	if err := CheckRate(inRate); err != nil {
		return nil, err
	}
	if err := CheckRate(outRate); err != nil {
		return nil, err
	}
	if channels <= 0 {
		return nil, fault.New(fault.KindAudio, fault.Fail, "audio", 0, -1,
			"invalid channel count %d", channels)
	}
	return &Resampler{InRate: inRate, OutRate: outRate, Channels: channels}, nil
}

// Process 处理一个输入块（交错采样），返回产生的输出块。
// 非整帧尾块返回可定位错误。
func (r *Resampler) Process(samples []float64) ([]float64, error) {
	ch := r.Channels
	if len(samples)%ch != 0 {
		return nil, fault.New(fault.KindAudio, fault.Fail, "audio", 0, -1,
			"non-integral tail block: %d samples not multiple of %d channels", len(samples), ch)
	}
	frames := int64(len(samples) / ch)
	if frames == 0 {
		return nil, nil
	}
	var out []float64
	for {
		// 输出采样 outIdx 的输入位置（有理数，避免浮点漂移）：
		// p = outIdx * InRate / OutRate
		num := r.outIdx * int64(r.InRate)
		pos := num / int64(r.OutRate)
		rem := num % int64(r.OutRate)
		if pos >= r.inBase+frames-1 {
			break // 需要下一块的帧才能插值
		}
		i := pos - r.inBase
		var s0, s1 []float64
		if i < 0 {
			if !r.havePrev {
				r.outIdx++
				continue
			}
			s0 = r.prev
			s1 = samples[:ch]
		} else {
			s0 = samples[i*int64(ch) : (i+1)*int64(ch)]
			if i+1 < frames {
				s1 = samples[(i+1)*int64(ch) : (i+2)*int64(ch)]
			} else {
				s1 = s0 // 块尾：用当前帧保持
			}
		}
		frac := float64(rem) / float64(r.OutRate)
		for c := 0; c < ch; c++ {
			out = append(out, s0[c]+frac*(s1[c]-s0[c]))
		}
		r.outIdx++
	}
	r.prev = append(r.prev[:0], samples[(frames-1)*int64(ch):frames*int64(ch)]...)
	r.havePrev = true
	r.inBase += frames
	return out, nil
}

// OutSamples 返回已产生的输出采样数（每声道）。
func (r *Resampler) OutSamples() int64 { return r.outIdx }

// OutStart 返回第 n 个输出采样在统一时钟上的时间。
func (r *Resampler) OutStart(n int64) clock.Ticks {
	return clock.SamplesToTicks(n, r.OutRate)
}
