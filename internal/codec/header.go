// Package codec 实现首批编解码适配器：JPEG 帧 PES 与 PCM 音频 PES。
//
// 为保证纯本地、确定性与无外部样例依赖，JPEG 适配器承载的是带自定义
// 帧头的标准 JPEG 字节（由 imaging 离屏编码产生），PCM 适配器承载
// 8 kHz 单声道 16-bit 小端样本。帧头统一携带魔数、序号、关键帧标记、
// 尺寸与长度，解码时逐项校验，越界长度一律拒绝。
package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// FrameHeader 是 JPEG/PCM PES 负载前的固定 24 字节私有帧头。
//
//	魔数 4B | 版本 2B | 序号 4B | 标志 1B | 宽 4B | 高 4B | 负载长 4B | 保留 1B
const (
	FrameHeaderLen = 24
	magic0         = 0x54 // 'T'
	magic1         = 0x53 // 'S'
	magic2         = 0x50 // 'P'
	magic3         = 0x46 // 'F'
	flagKey        = 0x01

	// 解码图像与缓冲的硬上限，防止畸形长度放大内存占用。
	MaxDimension = 640
	MaxPayload   = 1 << 20 // 1 MiB
)

// ErrFrameHeader 表示帧头魔数、版本或长度不合法。
var ErrFrameHeader = errors.New("codec: malformed frame header")

// Header 是解析后的帧头字段。
type Header struct {
	Version uint16
	Index   uint32
	Key     bool
	Width   uint32
	Height  uint32
	Length  uint32
}

// EncodeFrameHeader 把帧头写入 dst（必须至少 FrameHeaderLen 字节）。
func EncodeFrameHeader(dst []byte, h Header) {
	dst[0], dst[1], dst[2], dst[3] = magic0, magic1, magic2, magic3
	binary.BigEndian.PutUint16(dst[4:6], h.Version)
	binary.BigEndian.PutUint32(dst[6:10], h.Index)
	flags := byte(0)
	if h.Key {
		flags |= flagKey
	}
	dst[10] = flags
	binary.BigEndian.PutUint32(dst[11:15], h.Width)
	binary.BigEndian.PutUint32(dst[15:19], h.Height)
	binary.BigEndian.PutUint32(dst[19:23], h.Length)
	dst[23] = 0
}

// DecodeFrameHeader 校验并解析帧头，返回头与负载长度一致性结论。
func DecodeFrameHeader(buf []byte) (Header, error) {
	if len(buf) < FrameHeaderLen {
		return Header{}, fmt.Errorf("%w: %d bytes", ErrFrameHeader, len(buf))
	}
	if buf[0] != magic0 || buf[1] != magic1 || buf[2] != magic2 || buf[3] != magic3 {
		return Header{}, fmt.Errorf("%w: bad magic", ErrFrameHeader)
	}
	h := Header{
		Version: binary.BigEndian.Uint16(buf[4:6]),
		Index:   binary.BigEndian.Uint32(buf[6:10]),
		Key:     buf[10]&flagKey != 0,
		Width:   binary.BigEndian.Uint32(buf[11:15]),
		Height:  binary.BigEndian.Uint32(buf[15:19]),
		Length:  binary.BigEndian.Uint32(buf[19:23]),
	}
	if h.Version != 1 {
		return h, fmt.Errorf("%w: version=%d", ErrFrameHeader, h.Version)
	}
	if h.Length > MaxPayload {
		return h, fmt.Errorf("%w: payload %d > %d", ErrFrameHeader, h.Length, MaxPayload)
	}
	if h.Width > MaxDimension || h.Height > MaxDimension {
		return h, fmt.Errorf("%w: dimension %dx%d", ErrFrameHeader, h.Width, h.Height)
	}
	if uint64(FrameHeaderLen)+uint64(h.Length) != uint64(len(buf)) {
		return h, fmt.Errorf("%w: declared=%d actual=%d",
			ErrFrameHeader, h.Length, len(buf)-FrameHeaderLen)
	}
	return h, nil
}

// Payload 返回帧头之后的负载切片。
func Payload(buf []byte) []byte { return buf[FrameHeaderLen:] }
