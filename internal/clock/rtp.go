package clock

import (
	"errors"

	"github.com/pion/rtp"
)

// ErrRTPTimestamp 表示 RTP 包携带的时间戳无法纳入 33 位媒体时间轴。
var ErrRTPTimestamp = errors.New("clock: invalid RTP timestamp")

// RTPExtractor 从 RTP 包提取 32 位 RTP 时间戳并归一到统一 90 kHz 基准。
//
// 竞技直播的输入常经 RTP/ WebRTC 传输（pion/rtp），其时间戳为 32 位、
// 时钟频率随载荷变化。本结构负责把任意载荷时钟换算到 90 kHz，并把
// 32 位回绕交由 Unwrap33 统一处理（32 位是 33 位空间的合法子集）。
type RTPExtractor struct {
	// PayloadClockRate 是该载荷类型的 RTP 时钟频率（如视频 90000、音频 48000）。
	PayloadClockRate uint32
}

// NewRTPExtractor 校验载荷时钟并构造提取器。
func NewRTPExtractor(payloadClockRate uint32) (*RTPExtractor, error) {
	if payloadClockRate == 0 {
		return nil, ErrRTPTimestamp
	}
	return &RTPExtractor{PayloadClockRate: payloadClockRate}, nil
}

// ToTick90k 把单个 RTP 时间戳换算为 90 kHz Tick（四舍五入）。
// 结果仍保留在 33 位空间内，供 Unwrap33 展开回绕。
func (x *RTPExtractor) ToTick90k(rtpTimestamp uint32) (uint64, error) {
	if x.PayloadClockRate == 0 {
		return 0, ErrRTPTimestamp
	}
	scaled := uint64((uint64(rtpTimestamp)*uint64(TickRate) +
		uint64(x.PayloadClockRate)/2) / uint64(x.PayloadClockRate))
	if scaled >= WrapMod {
		// 32 位输入经上采样不会超过 2^32，必然落在 33 位空间内；
		// 越界只可能来自异常时钟配置，显式拒绝而非截断。
		return 0, ErrRTPTimestamp
	}
	return scaled, nil
}

// PacketTimestamp 从一个已解析的 RTP 包提取并归一时间戳。
func (x *RTPExtractor) PacketTimestamp(p *rtp.Packet) (uint64, error) {
	if p == nil {
		return 0, ErrRTPTimestamp
	}
	return x.ToTick90k(p.Timestamp)
}

// UnwrapRTP 便捷方法：先在原始 32 位载荷时钟上展开回绕，再把绝对时间
// 缩放到 90 kHz，返回统一时间轴上的绝对 Tick。先展开后缩放可避免
// 上采样掩盖 32 位回绕边界。
func (x *RTPExtractor) UnwrapRTP(timestamps []uint32) ([]Tick, int, error) {
	if x.PayloadClockRate == 0 {
		return nil, 0, ErrRTPTimestamp
	}
	abs, wraps, err := Unwrap32(timestamps)
	if err != nil {
		return nil, 0, err
	}
	out := make([]Tick, len(abs))
	for i, t := range abs {
		scaled := (int64(t)*int64(TickRate) + int64(x.PayloadClockRate)/2) /
			int64(x.PayloadClockRate)
		out[i] = Tick(scaled)
	}
	return out, wraps, nil
}
