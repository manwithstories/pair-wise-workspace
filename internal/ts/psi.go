package ts

import (
	"encoding/binary"
	"fmt"

	"mediatool/internal/clock"
)

// crc32mpeg 实现 MPEG-2 校验（多项式 0x04C11DB7，初值 0xFFFFFFFF，无反射无异或）。
var crc32mpegTable [256]uint32

func init() {
	for i := 0; i < 256; i++ {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		crc32mpegTable[i] = c
	}
}

// CRC32MPEG 计算整段（含 CRC 自身 4 字节）应为 0 的 MPEG CRC。
func CRC32MPEG(b []byte) uint32 {
	c := uint32(0xFFFFFFFF)
	for _, x := range b {
		c = c<<8 ^ crc32mpegTable[byte(c>>24)^x]
	}
	return c
}

// Section 是一个完整且 CRC 校验通过的 PSI 表段。
type Section struct {
	PID         uint16
	TableID     byte
	Version     int
	CurrentNext bool
	Offset      int64 // 起始包偏移
	Raw         []byte
}

// PMTEntry 描述节目映射中的一条流。
type PMTEntry struct {
	StreamType byte
	PID        uint16
}

// PMT 是解析后的节目映射表。
type PMT struct {
	Version int
	Entries []PMTEntry
}

// ParsePMT 从表段解析 PMT；非 PMT 表返回错误。
func ParsePMT(s Section) (PMT, error) {
	if s.TableID != 0x02 {
		return PMT{}, fmt.Errorf("table_id 0x%02x is not PMT", s.TableID)
	}
	b := s.Raw
	if len(b) < 12 {
		return PMT{}, fmt.Errorf("short PMT")
	}
	pmt := PMT{Version: s.Version}
	progInfoLen := int(binary.BigEndian.Uint16(b[10:12]) & 0x0FFF)
	pos := 12 + progInfoLen
	for pos+5 <= len(b)-4 {
		st := b[pos]
		epid := uint16(b[pos+1]&0x1F)<<8 | uint16(b[pos+2])
		esLen := int(binary.BigEndian.Uint16(b[pos+3:pos+5]) & 0x0FFF)
		pmt.Entries = append(pmt.Entries, PMTEntry{StreamType: st, PID: epid})
		pos += 5 + esLen
	}
	return pmt, nil
}

// PSIAssembler 跨包组装 PSI 表段并维护版本边界。
// 不完整或 CRC 失败的表不会覆盖最后一份有效映射。
type PSIAssembler struct {
	buf        map[uint16][]byte
	bufStart   map[uint16]int64
	lastValid  map[uint16]Section // PMT PID -> 最后有效表段
	lastVer    map[uint16]int
	patVersion int
	patSeen    bool
	PMTPIDs    map[uint16]bool
}

func NewPSIAssembler() *PSIAssembler {
	return &PSIAssembler{
		buf:       map[uint16][]byte{},
		bufStart:  map[uint16]int64{},
		lastValid: map[uint16]Section{},
		lastVer:   map[uint16]int{},
		PMTPIDs:   map[uint16]bool{},
	}
}

// Feed 消费一个包，返回新完成的表段与故障。
func (a *PSIAssembler) Feed(p Packet) ([]Section, clock.Faults) {
	if !p.HasPayload || (p.PID != 0 && !a.PMTPIDs[p.PID]) {
		return nil, nil
	}
	payload := p.Payload
	if p.PayloadStart {
		if len(payload) == 0 {
			return nil, nil
		}
		ptr := int(payload[0])
		if 1+ptr > len(payload) {
			return nil, clock.Faults{{Kind: clock.FaultPSIIncomplete, Offset: p.Offset,
				PID: int32(p.PID), Detail: "pointer field overruns payload"}}
		}
		payload = payload[1+ptr:]
		a.buf[p.PID] = a.buf[p.PID][:0]
		a.bufStart[p.PID] = p.Offset
	}
	a.buf[p.PID] = append(a.buf[p.PID], payload...)
	return a.drain(p.PID)
}

// drain 从缓冲中取出所有完整表段。
func (a *PSIAssembler) drain(pid uint16) ([]Section, clock.Faults) {
	var out []Section
	var faults clock.Faults
	buf := a.buf[pid]
	for len(buf) >= 3 {
		if buf[0] == 0xFF { // 包尾填充，不是新表段
			buf = nil
			break
		}
		secLen := int(binary.BigEndian.Uint16(buf[1:3]) & 0x0FFF)
		total := 3 + secLen
		if secLen == 0 || total > 1024 {
			faults = append(faults, clock.Fault{Kind: clock.FaultPSIIncomplete,
				Offset: a.bufStart[pid], PID: int32(pid),
				Detail: fmt.Sprintf("implausible section_length %d", secLen)})
			a.buf[pid] = nil
			return out, faults
		}
		if len(buf) < total {
			break // 等待后续包
		}
		raw := append([]byte(nil), buf[:total]...)
		buf = buf[total:]
		if CRC32MPEG(raw) != 0 {
			faults = append(faults, clock.Fault{Kind: clock.FaultPSIIncomplete,
				Offset: a.bufStart[pid], PID: int32(pid), Detail: "CRC mismatch"})
			continue
		}
		sec := Section{
			PID: pid, TableID: raw[0],
			Version:     int(raw[5] >> 1 & 0x1F),
			CurrentNext: raw[5]&0x01 != 0,
			Offset:      a.bufStart[pid], Raw: raw,
		}
		out = append(out, sec)
		faults = append(faults, a.apply(sec)...)
	}
	a.buf[pid] = buf
	return out, faults
}

// apply 维护版本边界：PAT 更新 PMT PID 集合；PMT 版本切换记录事件并更新最后有效映射。
func (a *PSIAssembler) apply(s Section) clock.Faults {
	if s.TableID == 0x00 && s.PID == 0 { // PAT
		a.patSeen = true
		if len(s.Raw) >= 12 {
			for pos := 8; pos+4 <= len(s.Raw)-4; pos += 4 {
				pid := uint16(s.Raw[pos+2]&0x1F)<<8 | uint16(s.Raw[pos+3])
				if s.Raw[pos] != 0 || s.Raw[pos+1] != 0 { // program_number != 0
					a.PMTPIDs[pid] = true
				}
			}
		}
		a.patVersion = s.Version
		return nil
	}
	if s.TableID != 0x02 {
		return nil
	}
	var faults clock.Faults
	if old, ok := a.lastVer[s.PID]; ok && s.Version != old {
		faults = append(faults, clock.Fault{Kind: clock.FaultPSIVersion,
			Offset: s.Offset, PID: int32(s.PID), Recovered: true,
			Detail: fmt.Sprintf("PMT version %d -> %d", old, s.Version)})
	}
	a.lastVer[s.PID] = s.Version
	a.lastValid[s.PID] = s
	return faults
}

// Flush 在输入结束时调用：缓冲区中残留的半截表段报告为不完整故障，
// 且不会覆盖最后一份有效映射。
func (a *PSIAssembler) Flush() clock.Faults {
	var faults clock.Faults
	for pid, buf := range a.buf {
		if len(buf) == 0 {
			continue
		}
		faults = append(faults, clock.Fault{Kind: clock.FaultPSIIncomplete,
			Offset: a.bufStart[pid], PID: int32(pid),
			Detail: fmt.Sprintf("incomplete section: %d bytes buffered at end of input", len(buf))})
		a.buf[pid] = nil
	}
	return faults
}

// LastValidPMT 返回某 PMT PID 最后一份有效映射；不存在时 ok=false。
func (a *PSIAssembler) LastValidPMT(pid uint16) (PMT, bool) {
	s, ok := a.lastValid[pid]
	if !ok {
		return PMT{}, false
	}
	pmt, err := ParsePMT(s)
	if err != nil {
		return PMT{}, false
	}
	return pmt, true
}
