package ts

import (
	"errors"
	"fmt"
	"hash/crc32"
)

// PSI 节布局常量（ISO/IEC 13818-1 §2.4.4）。
const (
	psiHeaderLen   = 8 // table_id .. last_section_number
	psiCRCLen      = 4
	psiMaxSection  = 1021
	psiSyntaxMask  = 0x10 // section_syntax_indicator 位
	psiVersionMask = 0x3E // version_number 在 byte6 的位置
	psiCurrentMask = 0x01 // current_next_indicator
)

// ErrCRC 表示 PSI 节 CRC32 校验失败（损坏节，不得切换映射）。
var ErrCRC = errors.New("ts: PSI CRC32 mismatch")

// ErrSection 表示节长度、指针或拼接字段不完整。
var ErrSection = errors.New("ts: malformed PSI section")

// Section 是一个通过 CRC 校验的完整 PSI 节。
type Section struct {
	TableID      uint8
	Syntax       bool
	Version      uint8
	Current      bool
	SectionNum   uint8
	LastSection  uint8
	TableIDExt   uint16
	Body         []byte // 含 CRC 的完整节字节
	Data         []byte // table_id 之后、CRC 之前的语法内容
}

// crcTable 是 MPEG-2 规定的 CRC32 多项式表。
var crcTable = crc32.MakeTable(0x04C11DB7)

// CRC32MPEG2 计算 MPEG-2 PSI 使用的 CRC32（初始 0xFFFFFFFF、输出不取反）。
func CRC32MPEG2(b []byte) uint32 {
	return crc32.Checksum(b, crcTable)
}

// parseSection 校验并解析单个完整节，CRC 错误包装为 ErrCRC。
func parseSection(raw []byte) (*Section, error) {
	if len(raw) < psiHeaderLen+psiCRCLen {
		return nil, fmt.Errorf("%w: short section %d", ErrSection, len(raw))
	}
	sectionLen := int(raw[1]&0x0F)<<8 | int(raw[2])
	if sectionLen < psiCRCLen+1 || 3+sectionLen > len(raw) {
		return nil, fmt.Errorf("%w: section_length=%d buf=%d", ErrSection, sectionLen, len(raw))
	}
	end := 3 + sectionLen
	given := uint32(raw[end-4])<<24 | uint32(raw[end-3])<<16 |
		uint32(raw[end-2])<<8 | uint32(raw[end-1])
	calc := CRC32MPEG2(raw[1 : end-psiCRCLen])
	if given != calc {
		return nil, fmt.Errorf("%w: got=%08x calc=%08x", ErrCRC, given, calc)
	}
	s := &Section{
		TableID:     raw[0],
		Syntax:      raw[1]&psiSyntaxMask != 0,
		TableIDExt:  uint16(raw[3])<<8 | uint16(raw[4]),
		Version:     (raw[5] & psiVersionMask) >> 1,
		Current:     raw[5]&psiCurrentMask != 0,
		SectionNum:  raw[6],
		LastSection: raw[7],
		Body:        raw[:end],
		Data:        raw[psiHeaderLen : end-psiCRCLen],
	}
	return s, nil
}

// SectionAssembler 处理 PUSI 指针字段并跨包拼接待校验节。
// 缓冲区受 MaxSectionBuffer 限制，超长直接拒绝。
type SectionAssembler struct {
	pending []byte
	started bool
}

// NewSectionAssembler 创建空的 PSI 节装配器。
func NewSectionAssembler() *SectionAssembler {
	return &SectionAssembler{}
}

// Push 输入一个 PSI PID 的包负载，返回本次完成的零个或多个完整节原始字节。
// 每个节在返回前不做 CRC 校验，由调用方调用 parseSection 决定接受/拒绝。
func (a *SectionAssembler) Push(payload []byte) ([][]byte, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var done [][]byte
	pos := 0
	if payload[0] == 0x00 && !a.started {
		// pointer_field=0：新节从下一字节开始。
		pos = 1
	} else if a.started {
		// 续包不含指针字段。
	} else if payload[0] != 0x00 {
		// 首包带 pointer_field，先跳过指针指向的填充区。
		pointer := int(payload[0])
		pos = 1 + pointer
		if pos >= len(payload) {
			return nil, fmt.Errorf("%w: pointer %d", ErrSection, pointer)
		}
	}
	a.started = true
	a.pending = append(a.pending, payload[pos:]...)
	if len(a.pending) > MaxSectionBuffer {
		a.Reset()
		return nil, fmt.Errorf("%w: > %d bytes", ErrLength, MaxSectionBuffer)
	}
	for len(a.pending) >= 3 {
		need := 3 + int(a.pending[1]&0x0F)<<8 + int(a.pending[2])
		if need > MaxSectionBuffer {
			a.Reset()
			return nil, fmt.Errorf("%w: declared %d", ErrLength, need)
		}
		if len(a.pending) < need {
			break
		}
		sec := make([]byte, need)
		copy(sec, a.pending[:need])
		done = append(done, sec)
		a.pending = a.pending[need:]
	}
	if len(a.pending) == 0 {
		a.Reset()
	}
	return done, nil
}

// Reset 清空装配状态。
func (a *SectionAssembler) Reset() {
	a.pending = nil
	a.started = false
}

// finalizeSection 构造节字节并附加正确 CRC，供生成器与测试复用。
func finalizeSection(tableID uint8, ext uint16, version uint8, current bool,
	sectionNum, lastSection uint8, data []byte) []byte {
	sectionLen := 5 + len(data) + psiCRCLen // 5 字节 ext..last_section_number
	raw := make([]byte, 3+sectionLen)
	raw[0] = tableID
	raw[1] = psiSyntaxMask | byte(sectionLen>>8)
	raw[2] = byte(sectionLen)
	raw[3] = byte(ext >> 8)
	raw[4] = byte(ext)
	raw[5] = (version&0x1F)<<1 | boolBit(current)
	raw[6] = sectionNum
	raw[7] = lastSection
	copy(raw[8:], data)
	crc := CRC32MPEG2(raw[1 : len(raw)-psiCRCLen])
	raw[len(raw)-4] = byte(crc >> 24)
	raw[len(raw)-3] = byte(crc >> 16)
	raw[len(raw)-2] = byte(crc >> 8)
	raw[len(raw)-1] = byte(crc)
	return raw
}

func boolBit(b bool) byte {
	if b {
		return 1
	}
	return 0
}
