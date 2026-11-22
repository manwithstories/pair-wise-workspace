package core

import (
	"mediacheck/internal/model"
)

// 定点实现使用 Q31 比例与整数相位，避免浮点累计；输出样本数用有理数核算，
// 把累计舍入误差限制在 1 个输出采样以内。
//
// 仅支持 44.1k/48k 互转与 48k 直通（见 model 白名单）。
type Resampler struct {
	inRate   uint32
	outRate  uint32
	phase    uint64 // 累计相位（Q32），始终对应全局输入帧号
	inOffset int64  // 已接收输入帧总数（全局基址）
}

func NewResampler(inRate, outRate uint32) (*Resampler, error) {
	if !model.SupportedRate(inRate) || !model.SupportedRate(outRate) {
		return nil, &model.MediaError{
			Category: model.ErrUnsupportedRate,
			Message:  "rate must be 44100 or 48000",
		}
	}
	return &Resampler{inRate: inRate, outRate: outRate}, nil
}

// ExpectedOutputFrames 用有理数给出输入帧数对应的精确输出帧数（向下取整），
// 调用方据此校验实际输出与清单时长，误差不超过 1 帧。
func ExpectedOutputFrames(inFrames int, inRate, outRate uint32) int64 {
	r := model.NewRational(int64(inFrames)*int64(outRate), int64(inRate))
	return r.Num / r.Den
}

// Process 对单声道样本序列做线性插值重采样。输入 44.1k→48k 时比例 160/147。
func (rs *Resampler) Process(in []int32) []int32 {
	if rs.inRate == rs.outRate || len(in) == 0 {
		out := make([]int32, len(in))
		copy(out, in)
		return out
	}
	// step = inRate/outRate，Q32 定点。
	step := uint64(uint64(rs.inRate)<<32) / uint64(rs.outRate)
	capOut := ExpectedOutputFrames(len(in), rs.inRate, rs.outRate) + 1
	out := make([]int32, 0, capOut)
	for {
		globalIdx := rs.phase >> 32
		localIdx := int64(globalIdx) - rs.inOffset
		if localIdx >= int64(len(in)) {
			break
		}
		if localIdx < 0 {
			rs.phase += step
			continue
		}
		frac := rs.phase & 0xFFFFFFFF
		v0 := in[localIdx]
		var v1 int32
		if int(localIdx)+1 < len(in) {
			v1 = in[localIdx+1]
		} else {
			v1 = v0 // 末样本保持，不读越界
		}
		// 线性插值：v0 + (v1-v0)*frac/2^32，全程 int64 防溢出。
		delta := int64(v1) - int64(v0)
		interp := int64(v0) + (delta*int64(frac))>>32
		out = append(out, int32(interp))
		rs.phase += step
	}
	rs.inOffset += int64(len(in))
	return out
}

// Reset 清零相位与输入累计（每个新轨道/新节目调用）。
func (rs *Resampler) Reset() { rs.phase = 0; rs.inOffset = 0 }
