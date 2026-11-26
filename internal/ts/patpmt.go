package ts

import "fmt"

// MPEG-TS 表 ID 与演示用流类型标识。
const (
	TableIDPAT uint8 = 0x00
	TableIDPMT uint8 = 0x02

	// 私有流类型区间：本项目用 0x80/0x81 标识 JPEG 帧与 PCM 音频，
	// 避开标准注册值，由适配器注册表解释。
	StreamTypeJPEG uint8 = 0x80
	StreamTypePCM  uint8 = 0x81
)

// PATProgram 是 PAT 中的一条 program_number → PMT PID 映射。
type PATProgram struct {
	ProgramNumber uint16
	PMTPID        uint16
}

// PMTStream 是 PMT 中的一条 elementary stream 描述。
type PMTStream struct {
	StreamType uint8
	PID        uint16
}

// PMT 是解析后的节目映射表。
type PMT struct {
	ProgramNumber uint16
	PCRPID        uint16
	Streams       []PMTStream
	Version       uint8
}

// ParsePAT 解析已通过 CRC 的 PAT 节。
func ParsePAT(s *Section) ([]PATProgram, error) {
	if s.TableID != TableIDPAT {
		return nil, fmt.Errorf("%w: table_id=0x%02x not PAT", ErrSection, s.TableID)
	}
	d := s.Data
	if len(d) < 4 {
		return nil, fmt.Errorf("%w: PAT too short", ErrSection)
	}
	// 跳过 4 字节 transport_stream_id/version 区已在头部，data 起于
	// program_number（PAT 无额外 descriptor 前缀）。
	var progs []PATProgram
	for len(d) >= 4 {
		num := uint16(d[0])<<8 | uint16(d[1])
		pid := uint16(d[2]&0x1F)<<8 | uint16(d[3])
		progs = append(progs, PATProgram{ProgramNumber: num, PMTPID: pid})
		d = d[4:]
	}
	if len(d) != 0 {
		return nil, fmt.Errorf("%w: PAT trailing %d bytes", ErrSection, len(d))
	}
	return progs, nil
}

// BuildPAT 构造带正确 CRC 的 PAT 节。
func BuildPAT(tsid uint16, version uint8, progs []PATProgram) []byte {
	data := make([]byte, 0, 4*len(progs))
	for _, p := range progs {
		data = append(data, byte(p.ProgramNumber>>8), byte(p.ProgramNumber))
		data = append(data, byte(p.PMTPID>>8)|0xE0, byte(p.PMTPID))
	}
	return finalizeSection(TableIDPAT, tsid, version, true, 0, 0, data)
}

// ParsePMT 解析已通过 CRC 的 PMT 节。
func ParsePMT(s *Section) (*PMT, error) {
	if s.TableID != TableIDPMT {
		return nil, fmt.Errorf("%w: table_id=0x%02x not PMT", ErrSection, s.TableID)
	}
	d := s.Data
	if len(d) < 4 {
		return nil, fmt.Errorf("%w: PMT too short", ErrSection)
	}
	pmt := &PMT{
		ProgramNumber: s.TableIDExt,
		PCRPID:         uint16(d[0]&0x1F)<<8 | uint16(d[1]),
		Version:        s.Version,
	}
	progInfoLen := int(d[2]&0x0F)<<8 | int(d[3])
	d = d[4:]
	if progInfoLen > len(d) {
		return nil, fmt.Errorf("%w: program_info_length=%d", ErrSection, progInfoLen)
	}
	d = d[progInfoLen:]
	for len(d) >= 5 {
		st := PMTStream{StreamType: d[0], PID: uint16(d[1]&0x1F)<<8 | uint16(d[2])}
		esInfoLen := int(d[3]&0x0F)<<8 | int(d[4])
		d = d[5:]
		if esInfoLen > len(d) {
			return nil, fmt.Errorf("%w: ES_info_length=%d", ErrSection, esInfoLen)
		}
		d = d[esInfoLen:]
		pmt.Streams = append(pmt.Streams, st)
	}
	if len(d) != 0 {
		return nil, fmt.Errorf("%w: PMT trailing %d bytes", ErrSection, len(d))
	}
	return pmt, nil
}

// BuildPMT 构造带正确 CRC 的 PMT 节。
func BuildPMT(programNumber uint16, version uint8, pcrPID uint16, streams []PMTStream) []byte {
	data := make([]byte, 0, 4+5*len(streams))
	data = append(data, byte(pcrPID>>8)|0xE0, byte(pcrPID), 0xF0, 0x00) // 无节目描述符
	for _, st := range streams {
		data = append(data, st.StreamType, byte(st.PID>>8)|0xE0, byte(st.PID), 0xF0, 0x00)
	}
	return finalizeSection(TableIDPMT, programNumber, version, true, 0, 0, data)
}

// FindStream 在 PMT 中按流类型查找 PID，未找到返回 false。
func (p *PMT) FindStream(streamType uint8) (uint16, bool) {
	for _, st := range p.Streams {
		if st.StreamType == streamType {
			return st.PID, true
		}
	}
	return 0, false
}
