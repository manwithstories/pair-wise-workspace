package capture

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"mediad/internal/fault"
)

// On-disk record kinds. RTCP and RTP share the same length/arrival/CRC frame.
const (
	KindRTP  uint8 = 1
	KindRTCP uint8 = 2

	Magic0 = 'M'
	Magic1 = 'D'
	Magic2 = 'R'
	Magic3 = '1'

	headerSize = 8
	crcSize    = 4
	// MaxRecord is far above an L16 5.1 10 ms packet (~5.3 KiB), while
	// preventing a corrupt length field from requesting a giant allocation.
	MaxRecord = 1 << 20
)

// Record is one validated or damaged framed capture record.
type Record struct {
	Offset      int64
	HeaderBytes [headerSize]byte
	Kind        uint8
	ArrivalNS   int64
	Packet      []byte
	CRC         uint32
	CRCValid    bool
	// DamageOffset is the located bit/byte when CRC localization succeeds.
	DamageOffset int64
}

// Scanner reads the big-endian length, arrival timestamp and CRC record stream.
type Scanner struct {
	r         io.Reader
	size      int64
	offset    int64
	maxRecord int
	Report    fault.Report
}

func NewScanner(r io.Reader, size int64) (*Scanner, error) {
	if err := fault.CheckInputSize(size); err != nil {
		return nil, fault.Usage("scan", err)
	}
	return &Scanner{r: r, size: size, maxRecord: MaxRecord}, nil
}

// Offset is the number of bytes consumed from the stream.
func (s *Scanner) Offset() int64 { return s.offset }

// Next returns the next complete record. A truncated tail is reported in
// s.Report and Next then returns io.EOF, preserving the valid prefix.
func (s *Scanner) Next() (*Record, error) {
	start := s.offset
	var head [headerSize]byte
	n, err := io.ReadFull(s.r, head[:])
	if err == io.EOF {
		return nil, io.EOF
	}
	if err == io.ErrUnexpectedEOF {
		s.Report.Add(fault.Damage{
			Offset: start, EndOffset: start + int64(n), Kind: "truncated_header",
			Message: fmt.Sprintf("partial %d-byte record header", n), Recoverable: true,
		})
		s.offset += int64(n)
		return nil, io.EOF
	}
	if err != nil {
		return nil, fault.AtOffset("scan", s.offset, err)
	}
	s.offset += headerSize

	if head[0] != Magic0 || head[1] != Magic1 || head[2] != Magic2 || head[3] != Magic3 {
		d := fault.Damage{Offset: start, Kind: "bad_magic", Message: "record magic does not match MDR1", Recoverable: false}
		s.Report.Add(d)
		return nil, fault.AtOffset("scan", start, fmt.Errorf("bad record magic"))
	}

	length := binary.BigEndian.Uint32(head[4:8])
	_, verr := fault.CheckFrameLength(length, 1+8, crcSize, s.maxRecord)
	if verr != nil {
		d := fault.Damage{Offset: start + 4, Kind: "bad_length", Message: verr.Error(), Recoverable: false}
		s.Report.Add(d)
		return nil, fault.AtOffset("scan", start+4, verr)
	}
	bodyLen := int(length)
	body := make([]byte, bodyLen)
	got, err := io.ReadFull(s.r, body)
	if err != nil {
		end, _ := fault.SafeAddOffset(start, headerSize+bodyLen)
		s.Report.Add(fault.Damage{
			Offset: start, EndOffset: end, Kind: "truncated_frame",
			Message: fmt.Sprintf("record declares %d body bytes, read %d", bodyLen, got), Recoverable: true,
		})
		s.offset += int64(got)
		return nil, io.EOF
	}
	s.offset += int64(bodyLen)

	kind := body[0]
	if kind != KindRTP && kind != KindRTCP {
		s.Report.Add(fault.Damage{Offset: start + headerSize, Kind: "unknown_kind", Message: fmt.Sprintf("kind %d", kind), Recoverable: false})
		return nil, fault.AtOffset("scan", start+headerSize, fmt.Errorf("unknown record kind %d", kind))
	}
	arrival := int64(binary.BigEndian.Uint64(body[1:9]))
	crc := binary.BigEndian.Uint32(body[bodyLen-crcSize:])
	covered := make([]byte, 0, headerSize+bodyLen-crcSize)
	covered = append(covered, head[:]...)
	covered = append(covered, body[:bodyLen-crcSize]...)
	valid := crc32.ChecksumIEEE(covered) == crc

	rec := &Record{
		Offset: start, HeaderBytes: head, Kind: kind, ArrivalNS: arrival,
		Packet: body[9 : bodyLen-crcSize], CRC: crc, CRCValid: valid, DamageOffset: -1,
	}
	if !valid {
		loc := locateSingleBitDamage(covered, crc, start)
		rec.DamageOffset = loc
		msg := fmt.Sprintf("CRC mismatch: stored %08x", crc)
		d := fault.Damage{Offset: start, Kind: "bad_crc", Message: msg, Recoverable: true}
		if loc >= 0 {
			d.Offset = loc
			d.Kind = "single_bit_crc_damage"
			d.Message = msg + "; located flipped bit"
			rec.DamageOffset = loc
		}
		s.Report.Add(d)
	}
	return rec, nil
}

// WriteRecord appends one framed record and returns its total frame size.
func WriteRecord(w io.Writer, kind uint8, arrivalNS int64, packet []byte) (int, error) {
	if kind != KindRTP && kind != KindRTCP {
		return 0, fmt.Errorf("invalid record kind %d", kind)
	}
	if len(packet) > MaxRecord-13 {
		return 0, fmt.Errorf("packet too large: %d", len(packet))
	}
	length := 1 + 8 + len(packet) + crcSize
	if length > int(^uint32(0)) {
		return 0, fmt.Errorf("record length overflow")
	}
	var head [headerSize]byte
	head[0], head[1], head[2], head[3] = Magic0, Magic1, Magic2, Magic3
	binary.BigEndian.PutUint32(head[4:8], uint32(length))

	body := make([]byte, length)
	body[0] = kind
	binary.BigEndian.PutUint64(body[1:9], uint64(arrivalNS))
	copy(body[9:], packet)
	crcAt := len(body) - crcSize
	crc := crc32.NewIEEE()
	_, _ = crc.Write(head[:])
	_, _ = crc.Write(body[:crcAt])
	binary.BigEndian.PutUint32(body[crcAt:], crc.Sum32())

	if _, err := w.Write(head[:]); err != nil {
		return 0, err
	}
	if _, err := w.Write(body); err != nil {
		return headerSize, err
	}
	return headerSize + len(body), nil
}

// locateSingleBitDamage brute-forces a one-bit mutation whose CRC matches.
// Generated CRC fixtures use exactly one flipped bit, giving an exact byte
// offset. It returns -1 when the defect is not a single-bit error.
func locateSingleBitDamage(covered []byte, wantCRC uint32, baseOffset int64) int64 {
	buf := make([]byte, len(covered))
	for off := range covered {
		for bit := uint(0); bit < 8; bit++ {
			copy(buf, covered)
			buf[off] ^= 1 << bit
			if crc32.ChecksumIEEE(buf) == wantCRC {
				// Require uniqueness to avoid reporting a misleading offset.
				second := false
				for off2 := range covered {
					maxBit := uint(8)
					if off2 == off {
						// Already-known candidate is skipped by testing only other bits.
					}
					for bit2 := uint(0); bit2 < maxBit; bit2++ {
						if off2 == off && bit2 == bit {
							continue
						}
						copy(buf, covered)
						buf[off2] ^= 1 << bit2
						if crc32.ChecksumIEEE(buf) == wantCRC {
							second = true
							break
						}
					}
					if second {
						break
					}
				}
				if !second {
					return baseOffset + int64(off)
				}
				return -1
			}
		}
	}
	return -1
}
