package ts

import (
	"encoding/binary"
	"fmt"

	"mediqc/internal/media"
)

const (
	PATPID          = 0x0000
	streamH264      = 0x1b
	streamAACADTS   = 0x0f
	psiHeaderMin    = 8
	psiCRCLength    = 4
	maxSectionTotal = 4096
)

// Section is a parsed MPEG-2 PSI section header.
type Section struct {
	TableID       byte
	Version       byte
	CurrentNext   bool
	SectionNumber byte
	LastNumber    byte
	Length        int
	TotalLength   int
	CRC           uint32
	CRCOK         bool
	Raw           []byte
}

// StreamInfo is one elementary stream entry in a PMT.
type StreamInfo struct {
	PID        uint16          `json:"pid"`
	StreamType byte            `json:"stream_type"`
	Codec      media.Codec     `json:"codec"`
	Kind       media.TrackKind `json:"kind"`
	Supported  bool            `json:"supported"`
}

// PMT is the accepted subset of a Program Map Table.
type PMT struct {
	ProgramNumber uint16       `json:"program_number"`
	Version       byte         `json:"version"`
	PCRPID        uint16       `json:"pcr_pid"`
	Streams       []StreamInfo `json:"streams"`
}

// PAT is the accepted subset of a Program Association Table.
type PAT struct {
	TransportStreamID uint16 `json:"transport_stream_id"`
	Version           byte   `json:"version"`
	PMTPID            uint16 `json:"pmt_pid"`
}

// CRC32MPEG computes the MPEG-2 PSI CRC (poly 0x04C11DB7).
func CRC32MPEG(data []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// ParseSection validates section length and CRC.
func ParseSection(raw []byte, limits media.Limits) (*Section, *media.Error) {
	if len(raw) < 3 {
		return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: "section shorter than 3 bytes"}
	}
	sectionLength := int(binary.BigEndian.Uint16(raw[1:3]) & 0x0fff)
	total := sectionLength + 3
	maxSection := limits.MaxSectionBytes
	if maxSection <= 0 || maxSection > maxSectionTotal {
		maxSection = maxSectionTotal
	}
	if sectionLength < 5 || total > maxSection {
		return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: fmt.Sprintf("invalid section length %d", sectionLength)}
	}
	if len(raw) < total {
		return nil, &media.Error{Code: media.CodeUnexpectedEOF, Component: "ts.psi", Message: "section spans incomplete capture"}
	}
	sec := &Section{
		TableID:     raw[0],
		Length:      sectionLength,
		TotalLength: total,
		Raw:         append([]byte(nil), raw[:total]...),
	}
	if raw[1]&0x80 != 0 && total >= 9 {
		sec.Version = (raw[5] >> 1) & 0x1f
		sec.CurrentNext = raw[5]&0x01 != 0
		sec.SectionNumber = raw[6]
		sec.LastNumber = raw[7]
	}
	if total >= 4 {
		sec.CRC = binary.BigEndian.Uint32(raw[total-4 : total])
		sec.CRCOK = CRC32MPEG(raw[:total-4]) == sec.CRC
	}
	if !sec.CRCOK {
		return sec, &media.Error{Code: media.CodeCRCInvalid, Component: "ts.psi", Offset: 0, Message: "PSI CRC mismatch"}
	}
	return sec, nil
}

// ParsePAT parses the first program in a PAT section.
func ParsePAT(sec *Section) (*PAT, *media.Error) {
	if sec == nil || sec.TableID != 0x00 {
		return nil, &media.Error{Code: media.CodeUnsupported, Component: "ts.psi", Message: "not a PAT section"}
	}
	raw := sec.Raw
	if sec.TotalLength < psiHeaderMin+psiCRCLength {
		return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: "PAT too short"}
	}
	pat := &PAT{TransportStreamID: binary.BigEndian.Uint16(raw[3:5]), Version: sec.Version}
	for off := 8; off+4 <= sec.TotalLength-4; off += 4 {
		program := binary.BigEndian.Uint16(raw[off : off+2])
		pid := binary.BigEndian.Uint16(raw[off+2:off+4]) & 0x1fff
		if program != 0 {
			pat.PMTPID = pid
			return pat, nil
		}
	}
	return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: "PAT contains no program"}
}

// ParsePMT parses elementary stream entries and maps supported codecs.
func ParsePMT(sec *Section) (*PMT, *media.Error) {
	if sec == nil || sec.TableID != 0x02 {
		return nil, &media.Error{Code: media.CodeUnsupported, Component: "ts.psi", Message: "not a PMT section"}
	}
	raw := sec.Raw
	if sec.TotalLength < 12+psiCRCLength {
		return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: "PMT too short"}
	}
	pmt := &PMT{
		ProgramNumber: binary.BigEndian.Uint16(raw[3:5]),
		Version:       sec.Version,
		PCRPID:        binary.BigEndian.Uint16(raw[8:10]) & 0x1fff,
	}
	infoLength := int(binary.BigEndian.Uint16(raw[10:12]) & 0x0fff)
	pos := 12 + infoLength
	end := sec.TotalLength - 4
	if pos > end {
		return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: "PMT program descriptors overflow"}
	}
	for pos < end {
		if pos+5 > end {
			return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: "truncated PMT stream entry"}
		}
		streamType := raw[pos]
		pid := binary.BigEndian.Uint16(raw[pos+1:pos+3]) & 0x1fff
		esInfoLength := int(binary.BigEndian.Uint16(raw[pos+3:pos+5]) & 0x0fff)
		pos += 5
		if pos+esInfoLength > end {
			return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: "PMT ES descriptors overflow"}
		}
		pos += esInfoLength
		info := StreamInfo{PID: pid, StreamType: streamType}
		switch streamType {
		case streamH264:
			info.Codec, info.Kind, info.Supported = media.CodecH264, media.TrackVideo, true
		case streamAACADTS:
			info.Codec, info.Kind, info.Supported = media.CodecAAC, media.TrackAudio, true
		default:
			info.Codec, info.Kind, info.Supported = media.Codec(fmt.Sprintf("type-0x%02x", streamType)), "", false
		}
		pmt.Streams = append(pmt.Streams, info)
	}
	if len(pmt.Streams) == 0 {
		return nil, &media.Error{Code: media.CodeCountInvalid, Component: "ts.psi", Message: "PMT contains no elementary streams"}
	}
	return pmt, nil
}

// Registry retains the last trusted PSI map and counts successful switches.
type Registry struct {
	PAT         *PAT
	TrustedPMT  *PMT
	PMTSwitches int
	RejectedCRC int
	RejectedVer int
}

// ApplyPAT stores a CRC-valid PAT.
func (r *Registry) ApplyPAT(raw []byte, limits media.Limits) (*PAT, *media.Error) {
	sec, err := ParseSection(raw, limits)
	if err != nil {
		r.RejectedCRC++
		return nil, err
	}
	pat, err := ParsePAT(sec)
	if err != nil {
		return nil, err
	}
	r.PAT = pat
	return pat, nil
}

// ApplyPMT switches only after complete CRC validation and a consistent version.
func (r *Registry) ApplyPMT(raw []byte, limits media.Limits) (*PMT, *media.Error) {
	sec, perr := ParseSection(raw, limits)
	if perr != nil {
		r.RejectedCRC++
		return nil, perr
	}
	candidate, perr := ParsePMT(sec)
	if perr != nil {
		return nil, perr
	}
	if r.TrustedPMT != nil {
		expected := byte((r.TrustedPMT.Version + 1) & 0x1f)
		if candidate.Version != r.TrustedPMT.Version && candidate.Version != expected {
			r.RejectedVer++
			return nil, &media.Error{
				Code: media.CodePSIVersion, Component: "ts.psi",
				Message: fmt.Sprintf("PMT version %d inconsistent with trusted %d", candidate.Version, r.TrustedPMT.Version),
			}
		}
		if candidate.Version == expected {
			r.PMTSwitches++
		}
	}
	r.TrustedPMT = candidate
	return candidate, nil
}

// SectionAssembler reassembles PSI sections across TS packets.
type SectionAssembler struct {
	buf []byte
}

// Push accepts one PSI packet payload. Complete sections are returned.
func (a *SectionAssembler) Push(pusi bool, payload []byte, limits media.Limits) ([][]byte, *media.Error) {
	var sections [][]byte
	if pusi {
		if len(payload) == 0 {
			return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: "PUSI packet without pointer"}
		}
		pointer := int(payload[0])
		if 1+pointer > len(payload) {
			return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: "PSI pointer beyond payload"}
		}
		a.buf = append(a.buf[:0], payload[1+pointer:]...)
	} else if len(a.buf) > 0 {
		a.buf = append(a.buf, payload...)
	} else {
		return nil, nil
	}

	maxSection := limits.MaxSectionBytes
	if maxSection <= 0 || maxSection > maxSectionTotal {
		maxSection = maxSectionTotal
	}
	for len(a.buf) >= 3 {
		if a.buf[0] == 0xff {
			a.buf = nil
			break
		}
		total := int(binary.BigEndian.Uint16(a.buf[1:3])&0x0fff) + 3
		if total < 3 || total > maxSection {
			a.buf = nil
			return sections, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.psi", Message: "reassembled PSI length invalid"}
		}
		if len(a.buf) < total {
			break
		}
		section := append([]byte(nil), a.buf[:total]...)
		sections = append(sections, section)
		a.buf = append(a.buf[:0], a.buf[total:]...)
	}
	return sections, nil
}
