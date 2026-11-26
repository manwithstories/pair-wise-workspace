package ts

import (
	"errors"
	"fmt"
)

// ErrTruncated 表示包体在声明字段结束前耗尽。
var ErrTruncated = errors.New("ts: packet truncated")

// ParsePacket 解析单个 188 字节包，提取头字段、适配域 PCR 与负载切片。
// 任何越界长度都返回 ErrLength/ErrTruncated，绝不读取包外字节。
func ParsePacket(raw []byte, order int) (*Packet, error) {
	if len(raw) != PacketSize188 {
		return nil, ErrSize
	}
	if raw[0] != SyncByte {
		return nil, ErrSync
	}
	p := &Packet{
		Raw:         raw,
		Order:       order,
		AdaptLength: -1,
		PCR:         -1,
	}
	p.PUSI = raw[1]&0x40 != 0
	p.Priority = raw[1]&0x20 != 0
	p.PID = uint16(raw[1]&0x1F)<<8 | uint16(raw[2])
	p.Scrambling = raw[3] >> 6
	afc := (raw[3] >> 4) & 0x03
	p.Adaptation = afc&0b10 != 0
	p.HasPayload = afc&0b01 != 0
	p.CC = raw[3] & 0x0F

	pos := 4
	if p.Adaptation {
		if pos >= PacketSize188 {
			return nil, ErrTruncated
		}
		adaptLen := int(raw[pos])
		p.AdaptLength = adaptLen
		if adaptLen > 0 {
			start := pos + 1
			end := start + adaptLen
			if end > PacketSize188 {
				return nil, fmt.Errorf("%w: adaptation %d", ErrLength, adaptLen)
			}
			flags := raw[start]
			// PCR_flag 是 flags 的第 4 位（0x10）。
			if flags&0x10 != 0 && adaptLen >= 7 {
				base := int64(raw[start+1])<<25 |
					int64(raw[start+2])<<17 |
					int64(raw[start+3])<<9 |
					int64(raw[start+4])<<1 |
					int64(raw[start+5]>>7)
				ext := int64(raw[start+5]&0x01)<<8 | int64(raw[start+6])
				// PCR = base*300 + ext，统一到 27 MHz Tick。
				p.PCR = base*300 + ext
			}
		}
		pos += 1 + adaptLen
	}
	if p.HasPayload {
		if pos > PacketSize188 {
			return nil, fmt.Errorf("%w: payload offset %d", ErrLength, pos)
		}
		p.Payload = raw[pos:]
	}
	return p, nil
}

// EncodePCR 将 27 MHz Tick 编码为 PCR 6 字节字段，写入 dst[start+1..start+6]
// 风格的适配域布局（dst 指向 flags 字节）。
func EncodePCR(dst []byte, atFlags int, pcr27 int64) {
	base := uint64(pcr27 / 300)
	ext := uint64(pcr27 % 300)
	dst[atFlags+1] = byte(base >> 25)
	dst[atFlags+2] = byte(base >> 17)
	dst[atFlags+3] = byte(base >> 9)
	dst[atFlags+4] = byte(base >> 1)
	dst[atFlags+5] = byte(base&0x01)<<7 | byte(ext>>8)
	dst[atFlags+6] = byte(ext)
}

// CCTracker 逐 PID 跟踪连续性计数，识别重复、跳变与乱序。
type CCTracker struct {
	lastCC    map[uint16]uint8
	lastOrder map[uint16]int
	have      map[uint16]bool
}

// NewCCTracker 创建跟踪器。
func NewCCTracker() *CCTracker {
	return &CCTracker{
		lastCC:    map[uint16]uint8{},
		lastOrder: map[uint16]int{},
		have:      map[uint16]bool{},
	}
}

// CCEvent 描述一次连续性检查结论。
type CCEvent struct {
	PID      uint16
	Got      uint8
	Want     uint8
	Duplicate bool // 同 CC 连续出现（允许的重复包）
	Gap      int    // 跳过的包数，0 表示连续
	Reversed bool   // 输入序号倒退（乱序）
}

// Observe 输入一个带负载的包并返回连续性事件。空包与仅适配域包不推进 CC。
func (c *CCTracker) Observe(p *Packet) CCEvent {
	ev := CCEvent{PID: p.PID, Got: p.CC}
	if p.IsNull() || !p.HasPayload {
		return ev
	}
	if !c.have[p.PID] {
		c.have[p.PID] = true
		c.lastCC[p.PID] = p.CC
		c.lastOrder[p.PID] = p.Order
		ev.Want = p.CC
		return ev
	}
	ev.Want = (c.lastCC[p.PID] + 1) & 0x0F
	if p.Order < c.lastOrder[p.PID] {
		ev.Reversed = true
	}
	switch {
	case p.CC == c.lastCC[p.PID]:
		ev.Duplicate = true
	default:
		delta := int(p.CC) - int(c.lastCC[p.PID])
		if delta < 0 {
			delta += 16
		}
		ev.Gap = delta - 1
	}
	c.lastCC[p.PID] = p.CC
	c.lastOrder[p.PID] = p.Order
	return ev
}
