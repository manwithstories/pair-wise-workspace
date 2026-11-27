// Package packet 实现媒资包事件的编码校验、RTP 序号/时间戳回绕与确定性去重。
//
// 事件记录是离线采集缓存的最小恢复单元：每条记录自带编码、序号、时间戳、
// 时长、到达时间与 CRC32 校验值，可独立序列化与校验。
package packet

import (
	"encoding/binary"
	"hash/crc32"

	"archivemedia/internal/media"
)

// MaxPayload 是单包载荷硬上限 256 KiB，超出一律拒收。
const MaxPayload = 256 * 1024

const (
	recordMagic   uint16 = 0xA17C
	recordVersion uint16 = 1
	headerLen            = 48
)

// Codec 标识包内音频编码。
type Codec uint16

const (
	CodecUnknown Codec = 0
	CodecOpus    Codec = 1
	CodecAAC     Codec = 2
)

// String 返回编码名。
func (c Codec) String() string {
	switch c {
	case CodecOpus:
		return "opus"
	case CodecAAC:
		return "aac"
	default:
		return "unknown"
	}
}

// Flag 定义事件标志位。
type Flag uint16

const (
	// FlagMissing 表示该记录是突发丢失后生成的占位区间，不含有效载荷。
	FlagMissing Flag = 1 << iota
)

// Event 是单个包事件的完整记录。
type Event struct {
	StreamID      uint32
	Epoch         uint32
	Seq           uint16 // RTP 16 位序号（回绕前原值）
	Codec         Codec
	Flags         Flag
	RTPTimestamp  uint64 // 已做 32 位回绕展开的时间戳（RTP 时钟刻度）
	ClockRate     uint32 // RTP 时钟率，如 48000
	DurationTicks uint32 // 包时长（RTP 时钟刻度）
	ArrivalMicros int64  // 到达时间（微秒，单调时钟域）
	ArrivalIndex  int    // 到达序，0 基
	Payload       []byte
	CRC           uint32
}

// Key 是按流、纪元、序号构成的去重键。
type Key struct {
	StreamID uint32
	Epoch    uint32
	Seq      uint16
}

// Key 返回事件的去重键。
func (e Event) Key() Key { return Key{e.StreamID, e.Epoch, e.Seq} }

// Missing 报告该事件是否为丢失占位。
func (e Event) Missing() bool { return e.Flags&FlagMissing != 0 }

// Location 把事件映射为统一错误模型中的原始位置。
func (e Event) Location() media.ErrorLocation {
	return media.ErrorLocation{
		StreamID:     e.StreamID,
		Epoch:        e.Epoch,
		Seq:          e.Seq,
		ArrivalIndex: e.ArrivalIndex,
		ByteOffset:   -1,
	}
}

// Validate 校验结构合法性：越界载荷、零时长、未知编码、非法时钟率。
func (e Event) Validate() error {
	loc := e.Location()
	if len(e.Payload) > MaxPayload {
		return media.E(media.KindPacketCorrupt, "packet.validate", loc,
			"载荷超过 256 KiB 上限", nil)
	}
	if e.Missing() {
		// 占位包允许空载荷，但时长必须显式给出，供时间线生成占位区间。
		if e.DurationTicks == 0 {
			return media.E(media.KindPacketCorrupt, "packet.validate", loc,
				"占位区间零时长", nil)
		}
		return nil
	}
	if e.Codec != CodecOpus && e.Codec != CodecAAC {
		return media.E(media.KindPacketCorrupt, "packet.validate", loc,
			"未知音频编码", nil)
	}
	if e.ClockRate == 0 {
		return media.E(media.KindPacketCorrupt, "packet.validate", loc,
			"RTP 时钟率为零", nil)
	}
	if e.DurationTicks == 0 {
		return media.E(media.KindPacketCorrupt, "packet.validate", loc,
			"零时长包", nil)
	}
	if len(e.Payload) == 0 {
		return media.E(media.KindPacketCorrupt, "packet.validate", loc,
			"有效包载荷为空", nil)
	}
	return nil
}

var crcTable = crc32.MakeTable(crc32.IEEE)

// MarshalBinary 输出 48 字节定长头 + 载荷的确定性字节串，末 4 字节为 CRC32。
func (e Event) MarshalBinary() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	buf := make([]byte, headerLen+len(e.Payload))
	binary.LittleEndian.PutUint16(buf[0:], recordMagic)
	binary.LittleEndian.PutUint16(buf[2:], recordVersion)
	binary.LittleEndian.PutUint32(buf[4:], e.StreamID)
	binary.LittleEndian.PutUint32(buf[8:], e.Epoch)
	binary.LittleEndian.PutUint16(buf[12:], e.Seq)
	binary.LittleEndian.PutUint16(buf[14:], uint16(e.Codec))
	binary.LittleEndian.PutUint16(buf[16:], uint16(e.Flags))
	binary.LittleEndian.PutUint16(buf[18:], 0) // reserved
	binary.LittleEndian.PutUint64(buf[20:], e.RTPTimestamp)
	binary.LittleEndian.PutUint32(buf[28:], e.DurationTicks)
	binary.LittleEndian.PutUint32(buf[32:], e.ClockRate)
	binary.LittleEndian.PutUint64(buf[36:], uint64(e.ArrivalMicros))
	binary.LittleEndian.PutUint32(buf[40:], uint32(len(e.Payload)))
	copy(buf[headerLen:], e.Payload)
	crc := crc32.Checksum(buf[0:44], crcTable)
	crc = crc32.Update(crc, crcTable, buf[headerLen:])
	binary.LittleEndian.PutUint32(buf[44:], crc)
	return buf, nil
}

// UnmarshalEvent 解析记录并验证魔数、版本、长度与 CRC。
func UnmarshalEvent(b []byte) (*Event, error) {
	if len(b) < headerLen {
		return nil, media.E(media.KindPacketCorrupt, "packet.decode-record",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"记录短于 48 字节定长头", nil)
	}
	if magic := binary.LittleEndian.Uint16(b[0:]); magic != recordMagic {
		return nil, media.E(media.KindPacketCorrupt, "packet.decode-record",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"记录魔数不匹配", nil)
	}
	if ver := binary.LittleEndian.Uint16(b[2:]); ver != recordVersion {
		return nil, media.E(media.KindPacketCorrupt, "packet.decode-record",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"记录版本不受支持", nil)
	}
	wantCRC := binary.LittleEndian.Uint32(b[44:])
	payloadLen := binary.LittleEndian.Uint32(b[44-4:])
	if int(payloadLen) != len(b)-headerLen {
		return nil, media.E(media.KindPacketCorrupt, "packet.decode-record",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"载荷长度字段与记录不一致", nil)
	}
	zeroed := make([]byte, len(b))
	copy(zeroed, b)
	binary.LittleEndian.PutUint32(zeroed[44:], 0)
	gotCRC := crc32.Checksum(zeroed[0:44], crcTable)
	gotCRC = crc32.Update(gotCRC, crcTable, zeroed[headerLen:])
	if gotCRC != wantCRC {
		return nil, media.E(media.KindPacketCorrupt, "packet.decode-record",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"CRC32 校验失败", nil)
	}
	e := &Event{
		StreamID:      binary.LittleEndian.Uint32(b[4:]),
		Epoch:         binary.LittleEndian.Uint32(b[8:]),
		Seq:           binary.LittleEndian.Uint16(b[12:]),
		Codec:         Codec(binary.LittleEndian.Uint16(b[14:])),
		Flags:         Flag(binary.LittleEndian.Uint16(b[16:])),
		RTPTimestamp:  binary.LittleEndian.Uint64(b[20:]),
		DurationTicks: binary.LittleEndian.Uint32(b[28:]),
		ClockRate:     binary.LittleEndian.Uint32(b[32:]),
		ArrivalMicros: int64(binary.LittleEndian.Uint64(b[36:])),
		ArrivalIndex:  -1,
		Payload:       append([]byte(nil), b[headerLen:]...),
		CRC:           wantCRC,
	}
	return e, e.Validate()
}

// ExtendSeq 依据上一展开序号 prev，把 16 位 RTP 序号展开为 64 位单调序号。
// 采用 RFC 3550 附录 A.1 的回绕语义：先在 prev 所在周期取候选值，
// 再按有向序号差决定前进一周期、后退一周期或保持当前周期。
func ExtendSeq(prev int64, seq uint16) int64 {
	base := prev &^ 0xFFFF
	cand := base | int64(seq)
	delta := int16(uint16(seq) - uint16(prev&0xFFFF))
	switch {
	case delta == 0:
		return prev
	case delta > 0 && cand <= prev:
		// 序号前进但候选落在旧周期，说明发生正向回绕。
		return cand + (1 << 16)
	case delta < 0 && cand > prev:
		// 序号后退但候选落在新周期，说明是迟到的上一周期包。
		return cand - (1 << 16)
	default:
		return cand
	}
}

// SeqDistance 返回 a 相对 b 的有向序号差（a-b，模 65536），区间 [-32768,32767]。
func SeqDistance(a, b uint16) int16 { return int16(a - b) }

// TSUnwrapper 维护 32 位 RTP 时间戳到 64 位的回绕展开状态。
type TSUnwrapper struct {
	last uint64
	have bool
}

// Push 输入原始 32 位时间戳，返回展开后的 64 位时间戳。
func (u *TSUnwrapper) Push(ts uint32) uint64 {
	if !u.have {
		u.have = true
		u.last = uint64(ts)
		return u.last
	}
	delta := int32(ts - uint32(u.last)) // 利用回绕算术得到有向差
	u.last += uint64(int64(delta))
	return u.last
}

// Last 返回最近一次展开结果；未输入过时返回 0,false。
func (u *TSUnwrapper) Last() (uint64, bool) { return u.last, u.have }
