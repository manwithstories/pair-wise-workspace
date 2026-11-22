// Package rtp 提供基于 pion/rtp 的传输适配器：把规范化后的 fMP4 分片负载
// 打包为 RTP 包并可逆拆包，验证传输层与容器层共享同一套样本/错误模型。
// 纯本地内存运行，无网络套接字依赖。
package rtp

import (
	"fmt"

	"github.com/pion/rtp"

	"mediacheck/internal/model"
)

const (
	// PayloadType 为本地动态负载类型（96-127 区间），不与静态类型冲突。
	PayloadType uint8  = 96
	clockRate   uint32 = 48000
	maxPayload         = 1400 // 远低于常见 MTU，避免 IP 分片
	rtpVersion         = 2
)

// Packetizer 把分片字节流按固定 MTU 打包为 RTP 包序列。
type Packetizer struct {
	SSRC       uint32
	nextSeq    uint16
	nextTS     uint32
	packetized int
}

func NewPacketizer(ssrc uint32) *Packetizer {
	return &Packetizer{SSRC: ssrc, nextTS: 0}
}

// Packetize 把一个分片的原始负载拆为若干 RTP 包，时间戳按已打包字节推进。
// marker 位置于该分片最后一个包。
func (p *Packetizer) Packetize(fragmentSeq uint32, payload []byte) ([]*rtp.Packet, error) {
	if len(payload) == 0 {
		return nil, &model.MediaError{Category: model.ErrShortDuration, Message: "empty fragment payload"}
	}
	var out []*rtp.Packet
	for off := 0; off < len(payload); off += maxPayload {
		end := off + maxPayload
		if end > len(payload) {
			end = len(payload)
		}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        rtpVersion,
				PayloadType:    PayloadType,
				SequenceNumber: p.nextSeq,
				Timestamp:      p.nextTS,
				SSRC:           p.SSRC,
				CSRC:           nil,
			},
			Payload: append([]byte{}, payload[off:end]...),
		}
		if end == len(payload) {
			pkt.Marker = true
		}
		out = append(out, pkt)
		p.nextSeq++
		p.nextTS += uint32(end - off)
		p.packetized++
	}
	// 首个 CSRC 扩展位承载 fMP4 分片序号（本地约定，便于逆向后对账）。
	if len(out) > 0 {
		out[0].CSRC = []uint32{uint32(fragmentSeq)}
	}
	return out, nil
}

// Depacketize 校验 RTP 包并重组原始分片负载；乱序/版本错误返回结构化错误。
func Depacketize(packets []*rtp.Packet) (fragmentSeq uint32, payload []byte, err error) {
	if len(packets) == 0 {
		return 0, nil, &model.MediaError{Category: model.ErrShortDuration, Message: "no rtp packets"}
	}
	var buf []byte
	var expectedSeq uint16 = packets[0].SequenceNumber
	var ssrc uint32 = packets[0].SSRC
	for i, pkt := range packets {
		if pkt.Version != rtpVersion {
			return 0, nil, &model.MediaError{Category: model.ErrTruncatedBox, Message: "bad rtp version"}
		}
		if pkt.SSRC != ssrc {
			return 0, nil, &model.MediaError{Category: model.ErrDuplicateConflict, Message: "ssrc mismatch within fragment"}
		}
		if pkt.SequenceNumber != expectedSeq {
			return 0, nil, &model.MediaError{
				Category: model.ErrFragmentGap,
				Message:  fmt.Sprintf("rtp sequence gap at packet %d", i),
			}
		}
		expectedSeq++
		buf = append(buf, pkt.Payload...)
	}
	if !packets[len(packets)-1].Marker {
		return 0, nil, &model.MediaError{Category: model.ErrTruncatedBox, Message: "fragment missing marker bit"}
	}
	if len(packets[0].CSRC) > 0 {
		fragmentSeq = packets[0].CSRC[0]
	}
	return fragmentSeq, buf, nil
}

// MarshalBatch 把 RTP 包序列化为可存储字节（每包 2 字节长度前缀 + 包体）。
func MarshalBatch(packets []*rtp.Packet) ([][]byte, error) {
	out := make([][]byte, 0, len(packets))
	for _, pkt := range packets {
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, &model.MediaError{Category: model.ErrWriteFailed, Message: err.Error()}
		}
		out = append(out, raw)
	}
	return out, nil
}

// UnmarshalBatch 解析 MarshalBatch 的产物。
func UnmarshalBatch(raw [][]byte) ([]*rtp.Packet, error) {
	out := make([]*rtp.Packet, 0, len(raw))
	for _, b := range raw {
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(b); err != nil {
			// pion 的错误常量未导出，统一映射为可定位的截断类别。
			return nil, &model.MediaError{Category: model.ErrTruncatedBox, Message: err.Error()}
		}
		out = append(out, pkt)
	}
	return out, nil
}

// ClockRate 返回 RTP 时钟频率，与内核统一 48k 时基对齐。
func ClockRate() uint32 { return clockRate }
