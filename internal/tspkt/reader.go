package tspkt

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astits"
)

var ErrNoSync = errors.New("tspkt: no confirmed packet synchronization")

const (
	tsBodySize    = astits.MpegTsPacketSize
	maxPacketSize = 204
)

type Stats struct {
	PacketSize       int
	Packets          int64
	ResyncEvents     int64
	TruncatedPackets int64
	TruncatedBytes   int64
}

type Packet struct {
	Index                  int64
	Offset                 int64
	Size                   int
	PID                    uint16
	PUSI                   bool
	CC                     uint8
	AdaptationFieldControl uint8
	HasPayload             bool
	Payload                []byte
	Discontinuity          bool
	RandomAccess           bool
	HasPCR                 bool
	PCR                    int64
	TransportError         bool
}

type Reader struct {
	br         *bufio.Reader
	size       int
	syncOffset int
	index      int64
	offset     int64
	buf        [maxPacketSize]byte
	packet     Packet
	stats      Stats
	eof        bool
	resyncing  bool
}

func NewReader(r io.Reader) (*Reader, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	rd := &Reader{br: br}
	data, err := br.Peek(1)
	if err == io.EOF || len(data) == 0 {
		return rd, nil
	}
	if err != nil {
		return nil, err
	}

	probe, err := br.Peek(maxPacketSize*5 + 4)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, err
	}
	size, syncOffset, start, ok := detectPacket(probe)
	if !ok {
		return nil, ErrNoSync
	}
	if start > 0 {
		if _, err := br.Discard(start); err != nil {
			return nil, err
		}
		rd.offset = int64(start)
		rd.stats.ResyncEvents++
	}
	rd.size = size
	rd.syncOffset = syncOffset
	rd.stats.PacketSize = size
	return rd, nil
}

func detectPacket(data []byte) (size, syncOffset, start int, ok bool) {
	type candidate struct{ size, syncOffset int }
	candidates := []candidate{{188, 0}, {192, 4}, {204, 0}}
	for pos := 0; pos < len(data); pos++ {
		for _, c := range candidates {
			if hasFiveSyncs(data, pos, c.size, c.syncOffset) {
				return c.size, c.syncOffset, pos, true
			}
		}
	}
	return 0, 0, 0, false
}

func hasFiveSyncs(data []byte, start, size, syncOffset int) bool {
	if start < 0 || size <= 0 {
		return false
	}
	for i := 0; i < 5; i++ {
		pos := start + syncOffset + i*size
		if pos >= len(data) || data[pos] != 0x47 {
			return false
		}
	}
	return true
}

func (r *Reader) Next() (*Packet, error) {
	if r.eof || r.size == 0 {
		return nil, io.EOF
	}
	for {
		data, err := r.br.Peek(r.size)
		if len(data) == 0 {
			r.eof = true
			return nil, io.EOF
		}
		if len(data) < r.size {
			r.stats.TruncatedPackets++
			r.stats.TruncatedBytes += int64(len(data))
			_, _ = r.br.Discard(len(data))
			r.offset += int64(len(data))
			r.eof = true
			return nil, io.EOF
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
		if data[r.syncOffset] == 0x47 {
			break
		}
		if !r.resyncing {
			r.stats.ResyncEvents++
			r.resyncing = true
		}
		if err := r.resync(); err != nil {
			return nil, err
		}
	}
	r.resyncing = false

	if _, err := io.ReadFull(r.br, r.buf[:r.size]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			r.stats.TruncatedPackets++
			r.eof = true
			return nil, io.EOF
		}
		return nil, err
	}
	offset := r.offset
	r.offset += int64(r.size)
	body := r.buf[:tsBodySize]
	if r.size == 192 {
		body = r.buf[4:192]
	}
	if err := r.parsePacket(body, offset); err != nil {
		return nil, err
	}
	r.stats.Packets++
	return &r.packet, nil
}

func (r *Reader) resync() error {
	probeLen := r.syncOffset + 5*r.size
	for {
		data, err := r.br.Peek(probeLen)
		if len(data) == 0 {
			r.eof = true
			return io.EOF
		}
		for start := 0; start < len(data); start++ {
			if hasFiveSyncs(data, start, r.size, r.syncOffset) {
				if start > 0 {
					if _, err := r.br.Discard(start); err != nil {
						return err
					}
					r.offset += int64(start)
				}
				return nil
			}
		}
		if len(data) < probeLen || errors.Is(err, io.EOF) {
			r.stats.TruncatedPackets++
			r.stats.TruncatedBytes += int64(len(data))
			_, _ = r.br.Discard(len(data))
			r.offset += int64(len(data))
			r.eof = true
			return io.EOF
		}
		if _, err := r.br.Discard(1); err != nil {
			return err
		}
		r.offset++
	}
}

func (r *Reader) parsePacket(body []byte, offset int64) error {
	if len(body) != tsBodySize || body[0] != 0x47 {
		return fmt.Errorf("tspkt: packet %d has invalid sync byte", r.index)
	}
	p := &r.packet
	*p = Packet{Index: r.index, Offset: offset, Size: r.size}
	p.TransportError = body[1]&0x80 != 0
	p.PUSI = body[1]&0x40 != 0
	p.PID = uint16(body[1]&0x1f)<<8 | uint16(body[2])
	p.AdaptationFieldControl = (body[3] >> 4) & 0x03
	p.CC = body[3] & 0x0f
	if p.AdaptationFieldControl == 0 {
		return fmt.Errorf("tspkt: packet %d has reserved adaptation_field_control", r.index)
	}

	payloadStart := 4
	if p.AdaptationFieldControl&0x02 != 0 {
		if len(body) < 5 {
			return fmt.Errorf("tspkt: packet %d has truncated adaptation field", r.index)
		}
		afLen := int(body[4])
		if afLen > tsBodySize-5 {
			return fmt.Errorf("tspkt: packet %d adaptation_field_length %d exceeds packet", r.index, afLen)
		}
		if afLen > 0 {
			if err := parseAdaptation(p, body[5:5+afLen]); err != nil {
				return fmt.Errorf("tspkt: packet %d: %w", r.index, err)
			}
		}
		payloadStart = 5 + afLen
	}
	if p.AdaptationFieldControl&0x01 != 0 && payloadStart < tsBodySize {
		p.HasPayload = true
		p.Payload = body[payloadStart:]
	}
	r.index++
	return nil
}

func parseAdaptation(p *Packet, af []byte) error {
	flags := af[0]
	p.Discontinuity = flags&0x80 != 0
	p.RandomAccess = flags&0x40 != 0
	if flags&0x10 == 0 {
		return nil
	}
	if len(af) < 7 {
		return errors.New("PCR flag set but adaptation field is too short")
	}
	base := int64(af[1])<<25 | int64(af[2])<<17 | int64(af[3])<<9 | int64(af[4])<<1 | int64(af[5]>>7)
	ext := int64(af[5]&0x01)<<8 | int64(af[6])
	if ext >= 300 {
		return fmt.Errorf("PCR extension %d is out of range", ext)
	}
	p.HasPCR = true
	p.PCR = base*300 + ext
	return nil
}

func (r *Reader) Stats() Stats {
	return r.stats
}
