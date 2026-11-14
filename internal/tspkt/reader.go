// Package tspkt reads MPEG-TS packets from 188-byte or 192-byte
// (4-byte timestamp prefix) wrapped streams, with in-place resync.
package tspkt

import (
	"errors"
	"io"
)

const (
	PacketSize = 188
	Wrap192    = 192
	SyncByte   = 0x47

	bufSize = 256 * 1024
)

// Packet is one parsed TS packet. Payload aliases the reader's internal
// buffer and is only valid until the next call to Next.
type Packet struct {
	Index  int64 // packet sequence number (0-based, excludes skipped garbage)
	Offset int64 // absolute byte offset of the 188-byte packet in the file

	PID                    uint16
	PayloadUnitStart       bool
	AdaptationFieldControl byte
	ContinuityCounter      byte
	DiscontinuityIndicator bool
	HasAdaptation          bool
	HasPayload             bool
	Payload                []byte
	Raw                    []byte // the original 188 bytes (owned copy)
}

// Reader streams packets from an io.Reader.
type Reader struct {
	src     io.Reader
	buf     []byte
	start   int // window start in buf
	end     int // window end (valid data) in buf
	base    int64
	pktSize int // 188 or 192
	prefix  int // 0 or 4
	eof     bool
	nextIdx int64

	BadSync   uint64 // resync events
	SyncBytes uint64 // bytes discarded while resyncing
	Malformed uint64 // adaptation overflow / empty payload
}

// NewReader sniffs the wrap format (188 or 192) and returns a Reader.
func NewReader(src io.Reader) (*Reader, error) {
	r := &Reader{src: src, buf: make([]byte, 0, bufSize)}
	if err := r.sniff(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reader) sniff() error {
	for len(r.buf) < Wrap192*4 && !r.eof {
		if err := r.fill(); err != nil {
			return err
		}
	}
	b := r.buf
	switch {
	case len(b) >= PacketSize*2 && b[0] == SyncByte && b[PacketSize] == SyncByte:
		r.pktSize, r.prefix = PacketSize, 0
	case len(b) >= Wrap192*2 && b[4] == SyncByte && b[4+Wrap192] == SyncByte:
		r.pktSize, r.prefix = Wrap192, 4
	default:
		// Fall back to 188 and let resync find the stream.
		r.pktSize, r.prefix = PacketSize, 0
	}
	return nil
}

// PacketSize returns the detected wrap size (188 or 192).
func (r *Reader) PacketSize() int { return r.pktSize }

// fill compacts the window and reads more data from the source.
func (r *Reader) fill() error {
	if r.start > 0 {
		copy(r.buf, r.buf[r.start:r.end])
		r.end -= r.start
		r.start = 0
	}
	n, err := r.src.Read(r.buf[r.end:cap(r.buf)])
	r.end += n
	r.buf = r.buf[:r.end]
	if errors.Is(err, io.EOF) {
		r.eof = true
		return nil
	}
	return err
}

// Next returns the next packet, or io.EOF.
func (r *Reader) Next() (*Packet, error) {
	for {
		if r.end-r.start < r.pktSize {
			if r.eof {
				if r.end > r.start {
					r.BadSync++
					r.SyncBytes += uint64(r.end - r.start)
					r.start = r.end
				}
				return nil, io.EOF
			}
			if err := r.fill(); err != nil {
				return nil, err
			}
			continue
		}
		p := r.start + r.prefix
		if r.buf[p] == SyncByte {
			raw := make([]byte, PacketSize)
			copy(raw, r.buf[p:p+PacketSize])
			pkt := r.parse(raw)
			pkt.Raw = raw
			pkt.Index = r.nextIdx
			pkt.Offset = r.base + int64(r.start) + int64(r.prefix)
			r.nextIdx++
			r.base += int64(r.pktSize)
			r.start += r.pktSize
			return pkt, nil
		}
		r.resync()
	}
}

// resync scans forward for the next plausible sync byte.
func (r *Reader) resync() {
	r.BadSync++
	for i := r.start + 1; i+r.prefix < r.end; i++ {
		if r.buf[i+r.prefix] != SyncByte {
			continue
		}
		// Confirm with the following packet's sync byte when possible.
		nxt := i + r.pktSize
		if nxt+r.prefix < r.end && r.buf[nxt+r.prefix] != SyncByte {
			continue
		}
		r.SyncBytes += uint64(i - r.start)
		r.base += int64(i - r.start)
		r.start = i
		return
	}
	// No sync found: discard everything but a tail and read more.
	if r.eof {
		r.SyncBytes += uint64(r.end - r.start)
		r.start = r.end
		return
	}
	keep := r.pktSize
	if r.end-r.start < keep {
		keep = r.end - r.start
	}
	drop := r.end - r.start - keep
	if drop == 0 {
		drop = 1 // guarantee progress
	}
	r.SyncBytes += uint64(drop)
	r.base += int64(drop)
	r.start += drop
}

func (r *Reader) parse(b []byte) *Packet {
	pkt := &Packet{
		PID:                    uint16(b[1]&0x1F)<<8 | uint16(b[2]),
		PayloadUnitStart:       b[1]&0x40 != 0,
		AdaptationFieldControl: (b[3] >> 4) & 0x03,
		ContinuityCounter:      b[3] & 0x0F,
	}
	pkt.HasAdaptation = pkt.AdaptationFieldControl == 2 || pkt.AdaptationFieldControl == 3
	pkt.HasPayload = pkt.AdaptationFieldControl == 1 || pkt.AdaptationFieldControl == 3
	payload := b[4:]
	malformed := false
	if pkt.HasAdaptation {
		afl := int(payload[0])
		if afl > len(payload)-1 {
			malformed = true
			payload = nil
		} else {
			if afl >= 1 {
				pkt.DiscontinuityIndicator = payload[1]&0x80 != 0
			}
			payload = payload[1+afl:]
		}
	}
	if pkt.HasPayload {
		if len(payload) == 0 {
			malformed = true
			pkt.HasPayload = false
		} else {
			pkt.Payload = payload
		}
	}
	if malformed {
		r.Malformed++
	}
	return pkt
}
