// Package ingest 解析 RTP 捕获数据，并完成序号去重、有界窗口乱序重排
// 与突发缺块的缺口期限管理。
package ingest

import (
	"github.com/pion/rtp"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/fault"
)

// ParsedPacket 是经 pion/rtp 解析并绑定时间基准后的 RTP 包。
type ParsedPacket struct {
	*rtp.Packet
	Frames int // 载荷包含的整采样帧数
}

// ParsePacket 解析单个 RTP 数据报，校验载荷是否按帧对齐。
// 拒绝不整帧载荷，并报告精确字节偏移与采样序号。
func ParsePacket(raw []byte, tb clock.Timebase) (*ParsedPacket, error) {
	if err := tb.Valid(); err != nil {
		return nil, fault.New(fault.KindMalformedInput, "", -1, -1, false,
			"invalid timebase").WithOp("rtp.parse").Wrap(err)
	}
	p := &rtp.Packet{}
	if err := p.Unmarshal(raw); err != nil {
		return nil, fault.New(fault.KindMalformedInput, "", -1, -1, true,
			"malformed RTP datagram").WithOp("rtp.parse").Wrap(err)
	}
	fs := tb.FrameSize()
	if len(p.Payload)%fs != 0 {
		rem := len(p.Payload) % fs
		goodFrames := uint64((len(p.Payload) - rem) / fs)
		return nil, fault.New(fault.KindMalformedInput, "",
			int64(len(p.Payload)-rem), int64(goodFrames), true,
			"RTP payload is not frame aligned").WithOp("rtp.parse")
	}
	return &ParsedPacket{Packet: p, Frames: len(p.Payload) / fs}, nil
}

// FrameSize 暴露时间基准帧大小，便于重排器做序号-采样换算。
func FrameSize(tb clock.Timebase) int { return tb.FrameSize() }
