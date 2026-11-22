// Package ts 解析 188 字节 MPEG-TS 包，跟踪连续计数并报告可定位故障。
package ts

import (
	"fmt"

	"mediatool/internal/clock"
)

const (
	// PacketSize 是标准 TS 包长度。
	PacketSize = 188
	// SyncByte 是包同步字节。
	SyncByte = 0x47
	// NullPID 是填充包 PID。
	NullPID = 0x1FFF
)

// Packet 是一个解析后的 TS 包。Adaptation 与 Payload 保存原始字节切片，
// 因此 Marshal 可以逐字节重建输入（用于往返一致性核验）。
type Packet struct {
	Offset       int64  // 在输入流中的字节偏移
	PID          uint16 // 13 位 PID
	PayloadStart bool   // payload_unit_start_indicator
	HasAdaptation bool
	HasPayload   bool
	CC           uint8  // continuity_counter（仅对有适配字段的包有意义）
	HasPCR       bool
	PCRBase      int64  // 33 位 PCR 基值（90kHz）
	Adaptation   []byte // 适配字段原始字节（含长度字节）
	Payload      []byte // 负载原始字节
}

// IsNull 报告是否为合法填充包。
func (p Packet) IsNull() bool { return p.PID == NullPID }

// Marshal 将解析结果重建为 188 字节包；对未修改的包保证与输入逐字节一致。
func (p Packet) Marshal() []byte {
	out := make([]byte, 0, PacketSize)
	out = append(out, SyncByte)
	b1 := byte(p.PID >> 8)
	if p.PayloadStart {
		b1 |= 0x40
	}
	out = append(out, b1, byte(p.PID))
	var afc byte
	switch {
	case p.HasAdaptation && p.HasPayload:
		afc = 0x30
	case p.HasAdaptation:
		afc = 0x20
	default:
		afc = 0x10
	}
	out = append(out, afc|p.CC&0x0F)
	out = append(out, p.Adaptation...)
	out = append(out, p.Payload...)
	return out
}

// ParseResult 是一次解析的输出：成功包列表与全部故障。
type ParseResult struct {
	Packets []Packet
	Faults  clock.Faults
}

// Parse 顺序解析 data 中的 TS 包。同步丢失与截断都以故障形式记录，
// 故障 Offset 指向首个异常包的字节偏移；解析在可恢复时继续。
func Parse(data []byte) ParseResult {
	res := ParseResult{}
	cc := NewCCTracker()
	n := len(data) / PacketSize
	for i := 0; i < n; i++ {
		off := int64(i * PacketSize)
		raw := data[i*PacketSize : (i+1)*PacketSize]
		pkt, err := parsePacket(raw, off)
		if err != nil {
			res.Faults = append(res.Faults, clock.Fault{
				Kind: clock.FaultSyncLoss, Offset: off, PID: clock.NoPosition,
				Detail: err.Error(),
			})
			continue
		}
		res.Packets = append(res.Packets, pkt)
		res.Faults = append(res.Faults, cc.Track(pkt)...)
	}
	if rem := len(data) % PacketSize; rem != 0 {
		res.Faults = append(res.Faults, clock.Fault{
			Kind: clock.FaultTruncatedPacket, Offset: int64(n * PacketSize), PID: clock.NoPosition,
			Detail: fmt.Sprintf("truncated tail: %d of %d bytes", rem, PacketSize),
		})
	}
	return res
}

func parsePacket(raw []byte, off int64) (Packet, error) {
	p := Packet{Offset: off}
	if raw[0] != SyncByte {
		return p, fmt.Errorf("sync byte 0x%02x", raw[0])
	}
	p.PayloadStart = raw[1]&0x40 != 0
	p.PID = uint16(raw[1]&0x1F)<<8 | uint16(raw[2])
	afc := raw[3] >> 4 & 0x3
	p.HasAdaptation = afc == 2 || afc == 3
	p.HasPayload = afc == 1 || afc == 3
	p.CC = raw[3] & 0x0F
	rest := raw[4:]
	if p.HasAdaptation {
		l := int(rest[0])
		if 1+l > len(rest) {
			return p, fmt.Errorf("adaptation field overruns packet")
		}
		p.Adaptation = rest[:1+l]
		flags := rest[1]
		if l >= 7 && flags&0x10 != 0 {
			b := rest[2:8]
			p.HasPCR = true
			p.PCRBase = int64(b[0])<<25 | int64(b[1])<<17 | int64(b[2])<<9 |
				int64(b[3])<<1 | int64(b[4])>>7
		}
		rest = rest[1+l:]
	}
	if p.HasPayload {
		p.Payload = rest
	}
	return p, nil
}

// CCEvent 描述一次连续计数判定。
type CCTracker struct {
	last    map[uint16]uint8
	hasLast map[uint16]bool
}

func NewCCTracker() *CCTracker {
	return &CCTracker{last: map[uint16]uint8{}, hasLast: map[uint16]bool{}}
}

// Track 按 PID 跟踪连续计数，区分可恢复缺口、重复包与不可恢复冲突。
// 判定规则（对有负载的包）：前进 1 正常；与上一包相同为重复；
// 前进 2..8 为可恢复缺口；其余（倒退或跳变过大）为不可恢复冲突。
func (t *CCTracker) Track(p Packet) clock.Faults {
	if p.IsNull() || !p.HasPayload {
		return nil
	}
	if !t.hasLast[p.PID] {
		t.last[p.PID], t.hasLast[p.PID] = p.CC, true
		return nil
	}
	last := t.last[p.PID]
	step := (int(p.CC) - int(last)) & 0xF
	mk := func(kind clock.FaultKind, recovered bool, detail string) clock.Fault {
		return clock.Fault{Kind: kind, Offset: p.Offset, PID: int32(p.PID), Detail: detail, Recovered: recovered}
	}
	switch {
	case step == 1:
		t.last[p.PID] = p.CC
		return nil
	case step == 0:
		return clock.Faults{mk(clock.FaultCCDuplicate, true,
			fmt.Sprintf("duplicate cc=%d", p.CC))}
	case step >= 2 && step <= 8:
		t.last[p.PID] = p.CC
		return clock.Faults{mk(clock.FaultCCGap, true,
			fmt.Sprintf("cc %d -> %d, lost %d packet(s)", last, p.CC, step-1))}
	default:
		return clock.Faults{mk(clock.FaultCCConflict, false,
			fmt.Sprintf("cc %d -> %d is not recoverable", last, p.CC))}
	}
}
