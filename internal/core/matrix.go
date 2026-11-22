package core

import (
	"mediacheck/internal/model"
)

// 声道顺序在所有布局中统一采用 SMPTE/ITU 标准下标：
// stereo:    [L, R]
// 5.1:       [L, R, C, LFE, Ls, Rs]
// 7.1:       [L, R, C, LFE, Ls, Rs, Lrs, Rrs]
//
// 矩阵系数为 Q15 定点增益（1<<15 = 1.0）。缺失的输入声道按配置补静音，
// 未知布局拒绝处理并回报轨道编号。

// ChannelMatrix 是 source→target 的显式混音矩阵：m[target][source]。
type ChannelMatrix struct {
	From   model.ChannelLayout
	To     model.ChannelLayout
	Gain   [][]int64 // Q15
	Fill   bool      // 缺失输入声道补静音（true）否则报错
}

const q15 = 1 << 15

// BuildMatrix 按目标布局构造显式矩阵。同布局直通；降级用标准 downmix；
// 升级把新增环绕/低音声道置静音（Fill=true）。
func BuildMatrix(from, to model.ChannelLayout, fillSilence bool) (*ChannelMatrix, error) {
	if from.Channels() == 0 || to.Channels() == 0 {
		return nil, &model.MediaError{Category: model.ErrUnknownLayout, Message: "unknown source or target layout"}
	}
	srcCh, dstCh := from.Channels(), to.Channels()
	m := &ChannelMatrix{From: from, To: to, Fill: fillSilence, Gain: make([][]int64, dstCh)}
	for i := range m.Gain {
		m.Gain[i] = make([]int64, srcCh)
	}
	switch {
	case from == to:
		for i := 0; i < srcCh; i++ {
			m.Gain[i][i] = q15
		}
	case from == model.Layout51 && to == model.LayoutStereo:
		downmix51ToStereo(m.Gain)
	case from == model.Layout71 && to == model.LayoutStereo:
		downmix71ToStereo(m.Gain)
	case from == model.Layout71 && to == model.Layout51:
		downmix71To51(m.Gain)
	case srcCh < dstCh:
		// 上混：共有声道直通，新增声道静音。
		for i := 0; i < srcCh; i++ {
			m.Gain[i][i] = q15
		}
	default:
		return nil, &model.MediaError{Category: model.ErrUnknownLayout,
			Message: "no explicit matrix for layout pair"}
	}
	return m, nil
}

// 标准 5.1→stereo 系数（Lo/Ro，ITU-R BS.775 常用近似）。
// c707 是 1/sqrt(2) 的 Q15 定点表示（0.7071*32768=23170）。
const c707 int64 = 23170

func downmix51ToStereo(g [][]int64) {
	g[0][0] = q15   // L
	g[0][2] = c707 // C→L
	g[0][4] = c707 // Ls→L
	g[0][3] = c707 / 2 // LFE→L
	g[1][1] = q15   // R
	g[1][2] = c707 // C→R
	g[1][5] = c707 // Rs→R
	g[1][3] = c707 / 2 // LFE→R
}

func downmix71ToStereo(g [][]int64) {
	downmix51ToStereo(g) // 前 6 声道沿用 5.1 规则
	g[0][6] = c707 // Lrs→L
	g[1][7] = c707 // Rrs→R
}

func downmix71To51(g [][]int64) {
	for i := 0; i < 6; i++ {
		g[i][i] = q15
	}
	// 后环绕并入侧环绕。
	half := int64(q15 / 2)
	g[4][6] = half
	g[5][7] = half
}

// Apply 对一帧执行矩阵混音。若输入声道数少于矩阵源声道且 Fill=false，返回错误。
func (m *ChannelMatrix) Apply(in []int32, trackID uint32) ([]int32, error) {
	if len(in) < len(m.Gain[0]) {
		if !m.Fill {
			return nil, &model.MediaError{
				Category: model.ErrUnknownLayout, TrackID: trackID,
				Message: "missing input channels and fill-silence disabled",
			}
		}
		padded := make([]int32, len(m.Gain[0]))
		copy(padded, in) // 缺失声道保持零（静音）
		in = padded
	}
	out := make([]int32, len(m.Gain))
	for t := range m.Gain {
		var acc int64
		for s, g := range m.Gain[t] {
			if g == 0 {
				continue
			}
			acc += (int64(in[s]) * g) >> 15
		}
		out[t] = sat16(acc)
	}
	return out, nil
}

func sat16(v int64) int32 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int32(v)
}
