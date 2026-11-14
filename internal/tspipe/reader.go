// Package tspipe reads 188-byte MPEG transport stream packets.
package tspipe

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astits"
)

const PacketSize = astits.MpegTsPacketSize

var (
	ErrNoSync          = errors.New("tspipe: no MPEG-TS sync byte found")
	ErrUnsupported192  = errors.New("tspipe: 192-byte encapsulated packets are unsupported; only 188-byte TS packets are accepted")
	ErrTruncatedPacket = errors.New("tspipe: truncated packet at end of input")
)

// Packet contains only the packet-level fields needed by the audit pipeline.
// Payload aliases the Reader's reusable packet buffer and is valid until the
// next call to Reader.Next.
type Packet struct {
	Offset        int64
	PID           uint16
	Continuity    uint8
	PUSI          bool
	TEI           bool
	Scrambled     bool
	HasAdaptation bool
	HasPayload    bool
	Discontinuity bool
	Payload       []byte
}

// Reader is a resynchronizing, streaming 188-byte TS packet reader.
type Reader struct {
	r          *bufio.Reader
	offset     int64
	endOffset  int64
	packet     [PacketSize]byte
	detected   bool
	seenPacket bool
	packets    int64
	syncEvents int64
	lostBytes  int64
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64*1024)}
}

func (r *Reader) Next() (*Packet, error) {
	if !r.detected {
		if err := r.detect192(); err != nil {
			return nil, err
		}
		r.detected = true
	}

	var window []byte
	for {
		var err error
		window, err = r.r.Peek(PacketSize)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, bufio.ErrBufferFull) {
				if len(window) == 0 {
					if !r.seenPacket {
						return nil, ErrNoSync
					}
					return nil, io.EOF
				}
				if len(window) < PacketSize {
					r.lostBytes += int64(len(window))
					if err := r.discard(len(window)); err != nil {
						return nil, err
					}
					if !r.seenPacket {
						return nil, ErrNoSync
					}
					return nil, fmt.Errorf("%w: %d trailing bytes", ErrTruncatedPacket, len(window))
				}
			}
			return nil, err
		}
		if window[0] == 0x47 {
			break
		}
		idx := bytes.IndexByte(window, 0x47)
		if idx < 0 {
			idx = len(window)
		}
		r.syncEvents++
		r.lostBytes += int64(idx)
		if err := r.discard(idx); err != nil {
			return nil, err
		}
	}

	copy(r.packet[:], window[:PacketSize])
	r.seenPacket = true
	offset := r.offset
	if err := r.discard(PacketSize); err != nil {
		return nil, err
	}
	p, err := parsePacket(r.packet[:], offset)
	if err != nil {
		r.syncEvents++
		return nil, err
	}
	r.packets++
	r.endOffset = offset + PacketSize
	return p, nil
}

func (r *Reader) discard(n int) error {
	if _, err := r.r.Discard(n); err != nil {
		return err
	}
	r.offset += int64(n)
	return nil
}

func (r *Reader) Packets() int64    { return r.packets }
func (r *Reader) SyncErrors() int64 { return r.syncEvents }
func (r *Reader) LostBytes() int64  { return r.lostBytes }
func (r *Reader) EndOffset() int64  { return r.endOffset }

func (r *Reader) detect192() error {
	const probe = 4 * PacketSize
	data, _ := r.r.Peek(probe)
	if len(data) < 3*PacketSize+1 {
		return nil
	}
	if syncEvery(data, PacketSize) {
		return nil
	}
	if syncEvery(data, 192) {
		return ErrUnsupported192
	}
	return nil
}

func syncEvery(data []byte, stride int) bool {
	for start := 0; start < stride; start++ {
		if start+3*stride >= len(data) || data[start] != 0x47 {
			continue
		}
		ok := true
		for i := 1; i < 4; i++ {
			if data[start+i*stride] != 0x47 {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func parsePacket(b []byte, offset int64) (*Packet, error) {
	flags := b[1]
	afc := (b[3] >> 4) & 0x3
	p := &Packet{
		Offset:     offset,
		PID:        uint16(flags&0x1f)<<8 | uint16(b[2]),
		Continuity: b[3] & 0x0f,
		PUSI:       flags&0x40 != 0,
		TEI:        flags&0x80 != 0,
		Scrambled:  b[3]&0xc0 != 0,
	}
	switch afc {
	case 1:
		p.HasPayload = true
		p.Payload = b[4:]
	case 2, 3:
		p.HasAdaptation = true
		afLen := int(b[4])
		if afLen > 183 || 5+afLen > PacketSize {
			return nil, fmt.Errorf("%w: invalid adaptation_field_length %d at offset %d", ErrTruncatedPacket, afLen, offset)
		}
		if afLen > 0 {
			p.Discontinuity = b[5]&0x80 != 0
		}
		payloadStart := 5 + afLen
		if afc == 3 && payloadStart < PacketSize {
			p.HasPayload = true
			p.Payload = b[payloadStart:]
		}
	default:
		return nil, fmt.Errorf("tspipe: reserved adaptation/control value at offset %d", offset)
	}
	return p, nil
}
