// Package timeline 按统一有理数时基把多声道包合成为单条时间线，
// 记录削波、缺失补偿与逐块时间对齐误差。
package timeline

import (
	"medpipe/internal/clock"
	"medpipe/internal/ingest"
)

// SourceRange 记录一段全局帧区间的来源。
type SourceRange struct {
	StartFrame int64
	EndFrame   int64 // 不含
	BlockID    int64
	Source     ingest.Source
}

// Mixer 累积所有通道提交的包并产出连续交错 PCM。
type Mixer struct {
	tb          clock.TimeBase
	channels    int
	bits        int
	totalFrames int64
	// 每帧每声道一个有符号 32 位累加器，混轨时饱和到 int16 域。
	acc         [][]int32
	placed      []bool
	ranges      []SourceRange
	clipped     int64
	missing     int64
	silenceFr   int64
	maxAlignEr  int64
	nextFrame   int64 // 已提交区间之后期望的下一帧坐标
}

// NewMixer 构造 totalFrames*channels 的有界时间线。
func NewMixer(tb clock.TimeBase, channels, bits int, totalFrames int64) (*Mixer, error) {
	if channels <= 0 || totalFrames <= 0 {
		return nil, clock.NewError(clock.CodeInvalidParam, -1, "timeline: 非法声道数/总帧数")
	}
	acc := make([][]int32, totalFrames)
	placed := make([]bool, totalFrames)
	for i := range acc {
		acc[i] = make([]int32, channels)
	}
	return &Mixer{tb: tb, channels: channels, bits: bits, totalFrames: totalFrames, acc: acc, placed: placed}, nil
}

// Apply 把一个包按 StartFrame 精确放置；越界与帧几何不符直接拒绝。
// 窗口外迟到包在 ingest 已拦截，这里再做一次坐标防御。
func (m *Mixer) Apply(p *ingest.Packet) error {
	if p == nil {
		return clock.NewError(clock.CodeInvalidParam, -1, "timeline: 空包")
	}
	if p.StartFrame < 0 || p.StartFrame+p.Frames > m.totalFrames {
		return clock.NewError(clock.CodeLatePacket, p.BlockID,
			"timeline: 包区间 [%d,%d) 越界（总长 %d）", p.StartFrame, p.StartFrame+p.Frames, m.totalFrames)
	}
	bpf := m.channels * (m.bits / 8)
	if p.Source == ingest.SourcePCM && len(p.PCM) != int(p.Frames)*bpf {
		return clock.NewError(clock.CodeInvalidParam, p.BlockID, "timeline: 包帧几何不符")
	}
	for f := int64(0); f < p.Frames; f++ {
		pos := p.StartFrame + f
		if p.Source == ingest.SourceSilence {
			// 静音只是占位：不写 placed，后续真实包仍可覆盖该位置。
			if !m.placed[pos] {
				m.missing += int64(m.channels)
				m.silenceFr++
			}
			continue
		}
		base := f * int64(bpf)
		for c := 0; c < m.channels; c++ {
			sample := readSample(p.PCM, base+int64(c)*2)
			sum := m.acc[pos][c] + sample
			if sum > 32767 {
				sum = 32767
				m.clipped++
			} else if sum < -32768 {
				sum = -32768
				m.clipped++
			}
			if sample == 32767 || sample == -32768 {
				m.clipped++ // 原始满量程样本同样计入削波证据
			}
			m.acc[pos][c] = sum
		}
		m.placed[pos] = true
	}
	m.ranges = append(m.ranges, SourceRange{
		StartFrame: p.StartFrame, EndFrame: p.StartFrame + p.Frames,
		BlockID: p.BlockID, Source: p.Source,
	})
	if p.Source == ingest.SourceSilence {
		// 静音占位同样推进坐标连续性。
		m.advance(p)
		return nil
	}
	// 时间对齐误差：包起始坐标相对“上一提交区间结束坐标”的间隙。
	// 正确的等长/短包序列该间隙恒为 0；任何重叠或跳变都会被捕获。
	if d := abs64(p.StartFrame - m.nextFrame); d > m.maxAlignEr {
		m.maxAlignEr = d
	}
	m.advance(p)
	return nil
}

// advance 推进期望下一帧坐标，仅接受连续或向后的包区间。
func (m *Mixer) advance(p *ingest.Packet) {
	end := p.StartFrame + p.Frames
	if end > m.nextFrame {
		m.nextFrame = end
	}
}

// PCM 导出饱和后的交错 16bit PCM，长度固定为 totalFrames*channels*2。
func (m *Mixer) PCM() []byte {
	out := make([]byte, m.totalFrames*int64(m.channels)*2)
	for f := int64(0); f < m.totalFrames; f++ {
		for c := 0; c < m.channels; c++ {
			v := m.acc[f][c]
			off := (f*int64(m.channels) + int64(c)) * 2
			out[off] = byte(uint16(v))
			out[off+1] = byte(uint16(v) >> 8)
		}
	}
	return out
}

// Stats 暴露合轨证据。
func (m *Mixer) Stats() (clipped, missingSamples, silenceFrames, maxAlignError int64, ranges []SourceRange) {
	return m.clipped, m.missing, m.silenceFr, m.maxAlignEr, m.ranges
}

// TotalFrames 返回时间线总采样帧数。
func (m *Mixer) TotalFrames() int64 { return m.totalFrames }

// Channels 返回声道数。
func (m *Mixer) Channels() int { return m.channels }

// IsPlaced 报告某帧是否已有真实或静音来源。
func (m *Mixer) IsPlaced(frame int64) bool { return frame >= 0 && frame < m.totalFrames && m.placed[frame] }

// readSample 读取小端 16bit 有符号样本。
func readSample(b []byte, off int64) int32 {
	u := uint16(b[off]) | uint16(b[off+1])<<8
	return int32(int16(u))
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
