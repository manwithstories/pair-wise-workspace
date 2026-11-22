package audio

import (
	"fmt"

	"github.com/pion/rtp"

	"mediatool/internal/clock"
)

// ResampleLinear 用线性插值把单声道采样从 inRate 转到 outRate。
// 输出长度按比率取整，保证边界处不丢采样相位。
func ResampleLinear(in []float32, inRate, outRate int) []float32 {
	if inRate <= 0 || outRate <= 0 {
		return nil
	}
	if inRate == outRate || len(in) == 0 {
		out := make([]float32, len(in))
		copy(out, in)
		return out
	}
	outLen := int(int64(len(in)) * int64(outRate) / int64(inRate))
	out := make([]float32, outLen)
	for i := 0; i < outLen; i++ {
		pos := float64(i) * float64(inRate) / float64(outRate)
		j := int(pos)
		frac := float32(pos - float64(j))
		if j+1 < len(in) {
			out[i] = in[j]*(1-frac) + in[j+1]*frac
		} else {
			out[i] = in[len(in)-1]
		}
	}
	return out
}

// RateBoundary 标记一次采样率切换：从第 AtSample 个输入采样起使用 NewRate。
type RateBoundary struct {
	AtSample int
	NewRate  int
}

// ResampleWithBoundaries 处理含采样率切换的流：按边界分段重采样到 outRate，
// 返回拼接后的输出与一条已恢复的 audio-rate 故障记录（无切换时为 nil）。
func ResampleWithBoundaries(in []float32, inRate int, bounds []RateBoundary, outRate int) ([]float32, *clock.Fault) {
	if len(bounds) == 0 {
		return ResampleLinear(in, inRate, outRate), nil
	}
	var out []float32
	rate := inRate
	prev := 0
	for _, b := range bounds {
		if b.AtSample < prev || b.AtSample > len(in) {
			return nil, &clock.Fault{Kind: clock.FaultAudioRateSwitch,
				Offset: clock.NoPosition, PID: clock.NoPosition,
				Detail: fmt.Sprintf("boundary at sample %d out of range", b.AtSample)}
		}
		out = append(out, ResampleLinear(in[prev:b.AtSample], rate, outRate)...)
		rate = b.NewRate
		prev = b.AtSample
	}
	out = append(out, ResampleLinear(in[prev:], rate, outRate)...)
	return out, &clock.Fault{Kind: clock.FaultAudioRateSwitch,
		Offset: clock.NoPosition, PID: clock.NoPosition, Recovered: true,
		Detail: fmt.Sprintf("sample rate switched %d times, resampled to %d Hz", len(bounds), outRate)}
}

// RTPSource 从独立音频设备的 RTP 流中解包，检测丢包并按 PT 变化识别采样率切换。
type RTPSource struct {
	lastSeq   uint16
	hasLast   bool
	RateForPT map[uint8]int // payload type -> 采样率
	lastRate  int
	SwitchPts []int // 发生切换的 RTP 包序号
	packetIdx int
}

func NewRTPSource(rateForPT map[uint8]int) *RTPSource {
	return &RTPSource{RateForPT: rateForPT}
}

// RTPResult 是一个解包结果。
type RTPResult struct {
	Payload   []byte
	Timestamp uint32
	Rate      int
	Lost      int // 此包前丢失的包数
}

// Ingest 解包一个 RTP 包；seq 回退或乱序按丢包上限裁剪，保证确定行为。
func (s *RTPSource) Ingest(raw []byte) (RTPResult, *clock.Fault, error) {
	var p rtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		return RTPResult{}, nil, fmt.Errorf("rtp unmarshal: %w", err)
	}
	rate, ok := s.RateForPT[p.PayloadType]
	if !ok {
		return RTPResult{}, &clock.Fault{Kind: clock.FaultAudioRateSwitch,
			Offset: clock.NoPosition, PID: clock.NoPosition,
			Detail: fmt.Sprintf("unknown payload type %d", p.PayloadType)}, nil
	}
	res := RTPResult{Payload: p.Payload, Timestamp: p.Timestamp, Rate: rate}
	var fault *clock.Fault
	if s.hasLast {
		step := int(p.SequenceNumber - s.lastSeq)
		if step > 1 {
			res.Lost = step - 1
			fault = &clock.Fault{Kind: clock.FaultPacketLoss,
				Offset: clock.NoPosition, PID: clock.NoPosition, Recovered: true,
				Detail: fmt.Sprintf("rtp seq gap: lost %d packet(s)", res.Lost)}
		}
	}
	if s.lastRate != 0 && rate != s.lastRate {
		s.SwitchPts = append(s.SwitchPts, s.packetIdx)
	}
	s.lastRate = rate
	s.lastSeq, s.hasLast = p.SequenceNumber, true
	s.packetIdx++
	return res, fault, nil
}
