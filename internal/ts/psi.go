package ts

import (
	"streamcut/internal/media"
)

// StreamType describes an elementary stream as signalled in the PMT.
type StreamType uint8

const (
	STH264        StreamType = 0x1B
	STPCMPrivate  StreamType = 0x06 // private data carrying PCM16LE here
	STAACADTS     StreamType = 0x0F // unsupported audio: report, never silence
)

// PMTStream is one elementary-stream entry of a committed PMT.
type PMTStream struct {
	StreamType StreamType
	ElementaryPID uint16
}

// ProgramState is the last fully validated PSI view. Only CRC-complete
// versions ever reach this struct.
type ProgramState struct {
	Version   uint8
	PMTPID    uint16
	PCRPID    uint16
	Streams   []PMTStream
	VideoPID  uint16
	AudioPID  uint16
	AudioKind media.StreamKind
}

// Section is one reassembled PSI section (pointer-field payload stripped).
type Section struct {
	TableID  byte
	Version  byte
	Number   byte
	Last     byte
	Body     []byte // bytes through the final CRC inclusive
	CRC      uint32
}

// crcTable is the CRC-32/MPEG-2 (poly 0x04C11DB7, init 0xFFFFFFFF, no
// reflection, no final xor) used by PSI sections.
var crcTable [256]uint32

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
		crcTable[i] = c
	}
}

// CRC32MPEG2 computes the PSI checksum over b.
func CRC32MPEG2(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, x := range b {
		crc = (crc << 8) ^ crcTable[byte(crc>>24)^x]
	}
	return crc
}

// SectionAssembler collects PSI sections from PID 0 (PAT) and the PMT PID.
// PSI packets carry a pointer field when PUSI is set; sections must end on a
// packet boundary.
type SectionAssembler struct {
	buf map[uint16][]byte
}

// NewSectionAssembler returns an empty assembler.
func NewSectionAssembler() *SectionAssembler {
	return &SectionAssembler{buf: map[uint16][]byte{}}
}

// Push feeds one PSI packet. Complete sections are returned in declaration
// order (a packet may finish one section and start another).
func (a *SectionAssembler) Push(p *Packet) ([]Section, error) {
	if !p.HasPayload {
		return nil, nil
	}
	pid := p.PID
	payload := p.Payload
	if p.PUSI {
		if len(payload) < 1 {
			return nil, media.Wrap(media.ETruncated, "ts.psi",
				"PUSI packet without pointer field", nil)
		}
		pointer := int(payload[0])
		payload = payload[1:]
		if pointer > len(payload) {
			return nil, media.Wrap(media.EUnrecoverable, "ts.psi",
				"pointer field past payload", nil)
		}
		var done []Section
		if len(a.buf[pid]) > 0 && pointer > 0 {
			prev := append(a.buf[pid], payload[:pointer]...)
			if s, ok := decodeSection(prev); ok {
				done = append(done, s)
			}
			a.buf[pid] = a.buf[pid][:0]
		}
		payload = payload[pointer:]
		if len(payload) > 0 {
			a.buf[pid] = append(a.buf[pid][:0], payload...)
		}
		if s, ok := decodeSection(a.buf[pid]); ok {
			done = append(done, s)
			a.buf[pid] = a.buf[pid][:0]
		}
		return done, nil
	}
	a.buf[pid] = append(a.buf[pid], payload...)
	if s, ok := decodeSection(a.buf[pid]); ok {
		a.buf[pid] = a.buf[pid][:0]
		return []Section{s}, nil
	}
	return nil, nil
}

// decodeSection returns a section once the buffered bytes cover the declared
// length. Truncated/undersized input yields ok=false until more data arrives.
func decodeSection(b []byte) (Section, bool) {
	if len(b) < 3 {
		return Section{}, false
	}
	secLen := int(b[1]&0x0F)<<8 | int(b[2]) // bytes after the length field
	end := 3 + secLen
	if secLen < 5 || len(b) < end {
		return Section{}, false
	}
	body := b[:end]
	crc := uint32(body[end-4])<<24 | uint32(body[end-3])<<16 |
		uint32(body[end-2])<<8 | uint32(body[end-1])
	return Section{
		TableID: b[0],
		Version: (b[5] >> 1) & 0x1F,
		Number:  b[6],
		Last:    b[7],
		Body:    append([]byte(nil), body...),
		CRC:     crc,
	}, true
}

// ValidCRC checks the section checksum, including the three CRC bytes
// themselves: a correct section hashes to zero.
func (s Section) ValidCRC() bool { return CRC32MPEG2(s.Body) == 0 }
