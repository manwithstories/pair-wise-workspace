// Package ts 实现 MPEG-TS 传输流的包识别、PSI 节解析与 PES 抽取，
// 覆盖 188/204 字节包长、PCR、连续性计数与同步边界。
package ts

import (
	"errors"
	"fmt"
)

// 包长与同步字节常量。
const (
	SyncByte      byte = 0x47
	PacketSize188      = 188
	PacketSize204      = 204
	RSCheckSize        = 16 // 204 = 188 + 16 字节 Reed-Solomon 校验尾
	MaxPID             = 0x1FFF
	NullPID            = 0x1FFF
)

// 适配器与夹具共用的 PID 分配（演示流固定布局）。
const (
	PIDPAT uint16 = 0x0000
	PIDCAT uint16 = 0x0001
)

// 解析器硬性限制，超出即拒绝，防止畸形长度耗尽内存。
const (
	MaxPacketBuffer  = 8 << 20 // 单次输入上限 8 MiB
	MaxSectionBuffer = 4 << 10 // PSI 节上限 4 KiB
	MaxPESBuffer     = 256 << 10
)

var (
	// ErrSync 表示在预期位置未找到 0x47 同步字节。
	ErrSync = errors.New("ts: sync byte missing")
	// ErrSize 表示包长既不是 188 也不是 204，或总长度不是整数倍。
	ErrSize = errors.New("ts: invalid packet size")
	// ErrLength 表示字段声明长度越过包/缓冲边界。
	ErrLength = errors.New("ts: length out of bounds")
)

// Packet 是解析后的单个 MPEG-TS 包（仅保留 188 字节主体）。
type Packet struct {
	PID         uint16
	CC          uint8
	PUSI        bool // payload_unit_start_indicator
	Adaptation  bool // adaptation_field_control 高位
	HasPayload  bool // adaptation_field_control 低位
	Priority    bool
	Scrambling  uint8
	AdaptLength int  // adaptation_field_length，不存在时为 -1
	PCR         int64 // 27 MHz PCR；不存在时为 -1
	Payload     []byte // 指向输入底层数组的切片，不拷贝
	Raw         []byte // 完整 188 字节原始包
	Order       int    // 输入流中的序号，乱序检测用
}

// HasPCR 报告适配域是否携带 PCR。
func (p *Packet) HasPCR() bool { return p.PCR >= 0 }

// IsNull 报告是否为填充空包（PID 0x1FFF）。
func (p *Packet) IsNull() bool { return p.PID == NullPID }

// DetectSize 依据同步字节的间距判断流的包长。
// 仅含单个包时按总长度判定；数据不足或同步缺失时返回错误。
func DetectSize(buf []byte) (int, error) {
	if len(buf) < PacketSize188 || buf[0] != SyncByte {
		return 0, ErrSync
	}
	if len(buf) >= PacketSize204 && buf[PacketSize204] == SyncByte {
		return PacketSize204, nil
	}
	if len(buf) > PacketSize188 && buf[PacketSize188] == SyncByte {
		return PacketSize188, nil
	}
	switch len(buf) {
	case PacketSize188, PacketSize204:
		return len(buf), nil
	default:
		return 0, ErrSize
	}
}

// SplitPackets 在原始字节流中定位同步边界并返回每个 188 字节包的切片。
// 204 字节流会自动剥掉 16 字节 RS 尾。若同步后出现长度错配，返回 ErrSize；
// 起始处找不到同步字节返回 ErrSync。
func SplitPackets(buf []byte) ([][]byte, int, error) {
	if len(buf) > MaxPacketBuffer {
		return nil, 0, fmt.Errorf("%w: %d > %d", ErrLength, len(buf), MaxPacketBuffer)
	}
	if len(buf) == 0 {
		return nil, 0, nil
	}
	offset := 0
	if buf[0] != SyncByte {
		offset = indexSync(buf)
		if offset < 0 {
			return nil, 0, ErrSync
		}
	}
	body := buf[offset:]
	if len(body) < PacketSize188 {
		return nil, offset, ErrSize
	}
	size := PacketSize188
	if len(body) >= PacketSize204 && body[PacketSize204] == SyncByte &&
		(len(body) == PacketSize204 || body[PacketSize188] != SyncByte) {
		size = PacketSize204
	}
	if len(body)%size != 0 {
		return nil, offset, fmt.Errorf("%w: %d not multiple of %d", ErrSize, len(body), size)
	}
	count := len(body) / size
	out := make([][]byte, count)
	for i := 0; i < count; i++ {
		raw := body[i*size : i*size+PacketSize188]
		if raw[0] != SyncByte {
			return nil, offset, fmt.Errorf("%w at packet %d", ErrSync, i)
		}
		out[i] = raw
	}
	return out, offset, nil
}

func indexSync(buf []byte) int {
	for i, b := range buf {
		if b == SyncByte {
			return i
		}
	}
	return -1
}
