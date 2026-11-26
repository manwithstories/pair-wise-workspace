// Package pes 重组 MPEG-TS 承载的 PES 包，并把传输层连续性缺口
// 传播为受影响轨道的可信状态变化。
package pes

import (
	"errors"
	"fmt"
)

// PES 起始码与头部常量（ISO/IEC 13818-1 §2.4.3.6）。
const (
	StartCode0 byte = 0x00
	StartCode1 byte = 0x00
	StartCode2 byte = 0x01

	MinHeaderLen    = 9
	MaxPESLength    = 200 * 1024
	PTSDTSHeaderLen = 5 // PTS 或 PTS+DTS 各占 5 字节
)

// 演示流使用的 stream_id（私有区间，避免与标准音视频冲突）。
const (
	StreamIDJPEG uint8 = 0xFD // private_stream_1 风格，承载 JPEG 帧 PES
	StreamIDPCM  uint8 = 0xBD // private_stream_2 风格，承载 PCM 块 PES
)

var (
	// ErrStartCode 表示 PUSI 负载未以 0x000001 起始码开头。
	ErrStartCode = errors.New("pes: start code missing")
	// ErrPESLength 表示 PES_packet_length 越过缓冲或超过上限。
	ErrPESLength = errors.New("pes: length out of bounds")
	// ErrHeader 表示 PES 头部标志或字段不完整。
	ErrHeader = errors.New("pes: malformed header")
	// ErrGap 表示重组期间收到连续性缺口，PES 内容不可信。
	ErrGap = errors.New("pes: continuity gap during assembly")
)

// Packet 是重组完成的 PES 包。
type Packet struct {
	StreamID  uint8
	PTS90k    int64 // 90 kHz 单位；-1 表示无 PTS
	DTS90k    int64
	Data      []byte // PES_packet_data（已去除头部）
	Truncated bool   // 流在 PES 结束前被截断
	Untrusted bool   // 重组过程中出现 CC 缺口
}

// Encode 构造单个 PES 包。pts90k 传负值表示不带 PTS。
func Encode(streamID uint8, pts90k int64, data []byte) []byte {
	hasPTS := pts90k >= 0
	ptsDtsFlags := byte(0x00)
	ptsDtsLen := 0
	if hasPTS {
		ptsDtsFlags = 0x02 // PTS_DTS_flags = 10（仅 PTS）
		ptsDtsLen = PTSDTSHeaderLen
	}
	// PES_header_data_length 只统计可选头字节（PTS/DTS 等），
	// 不含其前面的 3 字节固定标志。
	headerDataLen := ptsDtsLen
	pesHeaderLen := 3 + headerDataLen
	// PES_packet_length 统计其后字节数：标志头 3 + PES_header_data + 负载。
	length := pesHeaderLen + len(data)
	out := make([]byte, 0, MinHeaderLen+ptsDtsLen+len(data))
	out = append(out, StartCode0, StartCode1, StartCode2, streamID)
	out = append(out, byte(length>>8), byte(length))
	// 字节6: marker bits '10'；字节7: PTS_DTS_flags 在高 2 位；字节8: 头长。
	out = append(out, 0x80, ptsDtsFlags<<6, byte(headerDataLen))
	if hasPTS {
		out = appendPTS(out, 0x20, pts90k) // 0010 前缀 => 仅 PTS
	}
	out = append(out, data...)
	return out
}

func appendPTS(buf []byte, prefix byte, pts int64) []byte {
	// ISO/IEC 13818-1 Figure 2-20：'0010' 4 位前缀 + 33 位 PTS，
	// 分三段存放，每段末尾为 marker bit 1。
	return append(buf,
		prefix|byte((pts>>30)&0x07)<<1|0x01,
		byte(pts>>22),
		byte((pts>>14)&0xFE)|0x01,
		byte(pts>>7),
		byte((pts<<1)&0xFE)|0x01)
}

// parseHeader 解析 PES 头部并返回数据负载与 PTS/DTS。
func parseHeader(raw []byte) (*Packet, int, error) {
	if len(raw) < MinHeaderLen {
		return nil, 0, fmt.Errorf("%w: %d bytes", ErrHeader, len(raw))
	}
	if raw[0] != StartCode0 || raw[1] != StartCode1 || raw[2] != StartCode2 {
		return nil, 0, ErrStartCode
	}
	p := &Packet{StreamID: raw[3], PTS90k: -1, DTS90k: -1}
	declared := int(raw[4])<<8 | int(raw[5])
	if declared > MaxPESLength {
		return nil, 0, fmt.Errorf("%w: %d", ErrPESLength, declared)
	}
	ptsDtsFlags := (raw[7] >> 6) & 0x03
	headerLen := int(raw[8])
	if headerLen+MinHeaderLen > len(raw) {
		return nil, 0, fmt.Errorf("%w: header_data_length=%d", ErrHeader, headerLen)
	}
	pos := MinHeaderLen
	switch ptsDtsFlags {
	case 0x02: // 仅 PTS
		if headerLen < PTSDTSHeaderLen || pos+PTSDTSHeaderLen > len(raw) {
			return nil, 0, fmt.Errorf("%w: PTS fields", ErrHeader)
		}
		pts, err := decodeTimestamp(raw[pos:])
		if err != nil {
			return nil, 0, err
		}
		p.PTS90k = pts
		pos += PTSDTSHeaderLen
	case 0x03: // PTS + DTS
		if headerLen < 2*PTSDTSHeaderLen || pos+2*PTSDTSHeaderLen > len(raw) {
			return nil, 0, fmt.Errorf("%w: PTS/DTS fields", ErrHeader)
		}
		pts, err := decodeTimestamp(raw[pos:])
		if err != nil {
			return nil, 0, err
		}
		dts, err := decodeTimestamp(raw[pos+PTSDTSHeaderLen:])
		if err != nil {
			return nil, 0, err
		}
		p.PTS90k, p.DTS90k = pts, dts
		pos += 2 * PTSDTSHeaderLen
	case 0x00:
		// 无时间戳。
	default:
		return nil, 0, fmt.Errorf("%w: PTS_DTS_flags=%d", ErrHeader, ptsDtsFlags)
	}
	dataStart := MinHeaderLen + headerLen
	if dataStart > len(raw) {
		return nil, 0, fmt.Errorf("%w: data start %d", ErrHeader, dataStart)
	}
	p.Data = raw[dataStart:]
	// 校验声明总长，防止截断数据被当成完整 PES。
	if declared > 0 && 3+declared > len(raw) {
		return p, dataStart, fmt.Errorf("%w: declared=%d actual=%d",
			ErrPESLength, 3+declared, len(raw))
	}
	return p, dataStart, nil
}

func decodeTimestamp(b []byte) (int64, error) {
	if len(b) < 5 {
		return 0, fmt.Errorf("%w: timestamp short", ErrHeader)
	}
	if b[0]&0x01 == 0 || b[2]&0x01 == 0 || b[4]&0x01 == 0 {
		return 0, fmt.Errorf("%w: marker bits", ErrHeader)
	}
	pts := int64(b[0]>>1&0x07)<<30 |
		int64(b[1])<<22 |
		int64(b[2]>>1)<<15 |
		int64(b[3])<<7 |
		int64(b[4] >> 1)
	return pts, nil
}
