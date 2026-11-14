// Package ts reads MPEG-TS packets with byte-level synchronization recovery.
package ts

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

const (
	PacketSize188 = 188
	PacketSize192 = 192
	SyncByte      = 0x47
	NullPID       = 0x1fff
)

var ErrNoPacket = errors.New("ts: no packet found")

// Header is the parsed four-byte MPEG-TS header.
type Header struct {
	TransportErrorIndicator bool
	PayloadUnitStart        bool
	TransportPriority       bool
	PID                     uint16
	Scrambling              uint8
	AdaptationField         bool
	HasPayload              bool
	ContinuityCounter       uint8
}

// Adaptation contains the fields needed by offline auditing.
type Adaptation struct {
	Length                 int
	DiscontinuityIndicator bool
	RandomAccessIndicator  bool
	HasPCR                 bool
	PCRBase                int64
	PCRExtension           int64
}

// Packet is one validated TS packet. Payload aliases the reader's buffer and
// is valid until the next call to Next.
type Packet struct {
	Index   int64
	Size    int
	Header  Header
	Adapt   Adaptation
	Payload []byte
	ByteOff int64
}

// Stats describes packet boundaries and synchronization recovery.
type Stats struct {
	PacketSize   int
	PacketCount  int64
	SyncLosses   int64
	GarbageBytes int64
	BytesRead    int64
}

// Reader streams packets without loading the whole transport stream.
type Reader struct {
	br         *bufio.Reader
	packetSize int
	syncOffset int
	stats      Stats
	index      int64
	byteOff    int64
	done       bool
}

// NewReader detects 188-byte or 192-byte packet framing and skips leading
// garbage. A 192-byte packet is treated as a four-byte prefix followed by a
// normal 188-byte TS packet.
func NewReader(r io.Reader) (*Reader, error) {
	br := bufio.NewReaderSize(r, 256*1024)
	head, err := br.Peek(4096)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return nil, fmt.Errorf("ts: probing stream: %w", err)
	}
	start, size, syncOff, ok := detectFraming(head)
	if !ok {
		return nil, ErrNoPacket
	}
	rd := &Reader{br: br, packetSize: size, syncOffset: syncOff, stats: Stats{PacketSize: size}}
	if start > 0 {
		if _, err := br.Discard(start); err != nil {
			return nil, fmt.Errorf("ts: discarding leading garbage: %w", err)
		}
		rd.stats.SyncLosses = 1
		rd.stats.GarbageBytes = int64(start)
		rd.stats.BytesRead = int64(start)
	}
	return rd, nil
}

func detectFraming(b []byte) (start, size, syncOff int, ok bool) {
	bestScore := -1
	for off := 0; off < len(b); off++ {
		for _, candidate := range []struct {
			size, sync int
		}{{PacketSize188, 0}, {PacketSize192, 4}} {
			score := syncScore(b, off, candidate.size, candidate.sync)
			if score < 2 {
				continue
			}
			if score > bestScore {
				bestScore, start, size, syncOff, ok = score, off, candidate.size, candidate.sync, true
			}
		}
	}
	if ok {
		return
	}
	for off := 0; off < len(b); off++ {
		if off+PacketSize188 > len(b) {
			break
		}
		if b[off] == SyncByte {
			return off, PacketSize188, 0, true
		}
	}
	return
}

func syncScore(b []byte, off, size, syncOff int) int {
	score := 0
	for i := 0; i < 8; i++ {
		pos := off + syncOff + i*size
		if pos >= len(b) {
			break
		}
		if b[pos] != SyncByte {
			break
		}
		score++
	}
	return score
}

// Next returns the next synchronized packet. Garbage regions are skipped
// byte by byte and counted once as a sync loss; they never become packets.
func (r *Reader) Next() (*Packet, error) {
	if r.done {
		return nil, io.EOF
	}
next:
	for {
		buf, err := r.br.Peek(r.packetSize)
		if err != nil {
			if len(buf) > 0 {
				r.stats.SyncLosses++
				r.stats.GarbageBytes += int64(len(buf))
				r.stats.BytesRead += int64(len(buf))
				_, _ = r.br.Discard(len(buf))
			}
			r.done = true
			if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("ts: reading packet: %w", err)
		}
		if buf[r.syncOffset] == SyncByte {
			pkt := parsePacket(buf, r.packetSize, r.syncOffset, r.index)
			if pkt != nil {
				if _, err := r.br.Discard(r.packetSize); err != nil {
					return nil, fmt.Errorf("ts: consuming packet: %w", err)
				}
				pkt.ByteOff = r.byteOff
				r.byteOff += int64(r.packetSize)
				r.stats.BytesRead += int64(r.packetSize)
				r.stats.PacketCount++
				r.index++
				return pkt, nil
			}
		}
		if err := r.resync(); err != nil {
			r.done = true
			return nil, err
		}
		goto next
	}
}

func (r *Reader) resync() error {
	r.stats.SyncLosses++
	for {
		if _, err := r.br.Discard(1); err != nil {
			r.stats.SyncLosses--
			return io.EOF
		}
		r.stats.GarbageBytes++
		r.stats.BytesRead++
		buf, err := r.br.Peek(r.packetSize)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return io.EOF
			}
			return err
		}
		if buf[r.syncOffset] == SyncByte && (syncScore(buf, 0, r.packetSize, r.syncOffset) >= 2 || len(buf) < 2*r.packetSize) {
			return nil
		}
	}
}

// Stats returns a snapshot of reader counters.
func (r *Reader) Stats() Stats { return r.stats }

func parsePacket(buf []byte, size, syncOff int, index int64) *Packet {
	tsp := buf[syncOff:]
	if len(tsp) < PacketSize188 || tsp[0] != SyncByte {
		return nil
	}
	h := Header{
		TransportErrorIndicator: tsp[1]&0x80 != 0,
		PayloadUnitStart:        tsp[1]&0x40 != 0,
		TransportPriority:       tsp[1]&0x20 != 0,
		PID:                     uint16(tsp[1]&0x1f)<<8 | uint16(tsp[2]),
		Scrambling:              tsp[3] >> 6,
		AdaptationField:         tsp[3]&0x20 != 0,
		HasPayload:              tsp[3]&0x10 != 0,
		ContinuityCounter:       tsp[3] & 0x0f,
	}
	pkt := &Packet{Index: index, Size: size, Header: h}
	pos := 4
	if h.AdaptationField {
		if pos >= PacketSize188 {
			return nil
		}
		length := int(tsp[pos])
		pkt.Adapt.Length = length
		if pos+1+length > PacketSize188 {
			return nil
		}
		if length > 0 {
			flags := tsp[pos+1]
			pkt.Adapt.DiscontinuityIndicator = flags&0x80 != 0
			pkt.Adapt.RandomAccessIndicator = flags&0x40 != 0
			pkt.Adapt.HasPCR = flags&0x10 != 0
			if pkt.Adapt.HasPCR {
				base, ext := parsePCR(tsp[pos+2:])
				pkt.Adapt.PCRBase, pkt.Adapt.PCRExtension = base, ext
			}
		}
		pos += 1 + length
	}
	if h.HasPayload && pos < PacketSize188 {
		pkt.Payload = tsp[pos:]
	}
	return pkt
}

func parsePCR(b []byte) (int64, int64) {
	if len(b) < 6 {
		return 0, 0
	}
	base := int64(b[0])<<25 | int64(b[1])<<17 | int64(b[2])<<9 | int64(b[3])<<1 | int64(b[4])>>7
	ext := int64(b[4]&1)<<8 | int64(b[5])
	return base, ext
}
