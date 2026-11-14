package tspacket

import (
	"errors"
	"fmt"
	"io"
)

const (
	PacketSize = 188
	SyncByte   = byte(0x47)
	NullPID    = uint16(0x1fff)
	headerSize = 4
)

var (
	ErrEmptyInput = errors.New("tsprobe: input is empty")
	ErrNoSync     = errors.New("tsprobe: no TS sync byte found")
)

// Packet is a decoded MPEG-TS packet. The Packet, Raw and Payload are owned by
// Reader and remain valid only until the next call to ReadPacket.
type Packet struct {
	Raw            []byte
	Payload        []byte
	PID            uint16
	CC             uint8
	PUSI           bool
	TEI            bool
	HasPayload     bool
	HasAdaptation  bool
	AdaptationOnly bool
	Discontinuity  bool
	RandomAccess   bool
	HasPCR         bool
	PCRBase        uint64
	Duplicate      bool
	CCJump         bool
}

// Stats describes packet-level anomalies. Duplicate packets are reported but
// are not included in LostPackets.
type Stats struct {
	BytesRead        int64
	Packets          int64
	GarbageBytes     int64
	SyncResyncs      int64
	TEIPackets       int64
	AdaptationOnly   int64
	NullPackets      int64
	DuplicatePackets int64
	Discontinuities  int64
	CCJumps          int64
	CCResyncs        int64
	LostPackets      int64
}

type ccState struct {
	last  uint8
	valid bool
}

// Reader finds 188-byte packet boundaries in a possibly damaged stream.
type Reader struct {
	r           io.Reader
	buf         []byte
	start       int
	end         int
	eof         bool
	readErr     error
	anyPacket   bool
	ccThreshold uint8
	cc          map[uint16]ccState
	stats       Stats
	packet      Packet
}

// NewReader returns a packet reader that discards leading garbage and resumes
// at the next byte position that is followed by another sync byte 188 bytes
// later. ccThreshold is the largest accepted continuity-counter step.
func NewReader(r io.Reader, ccThreshold uint8) *Reader {
	if ccThreshold == 0 {
		ccThreshold = 8
	}
	return &Reader{
		r:           r,
		buf:         make([]byte, 64*1024),
		ccThreshold: ccThreshold,
		cc:          make(map[uint16]ccState),
	}
}

// Stats returns a snapshot of packet counters.
func (r *Reader) Stats() Stats { return r.stats }

// ReadPacket returns the next packet or io.EOF at the end of the stream.
func (r *Reader) ReadPacket() (*Packet, error) {
	for {
		avail, err := r.fill(PacketSize + 1)
		if avail == 0 {
			if r.stats.Packets == 0 {
				if err == nil || errors.Is(err, io.EOF) {
					return nil, ErrEmptyInput
				}
			}
			if errors.Is(err, io.EOF) || err == nil {
				return nil, io.EOF
			}
			if !r.anyPacket && errors.Is(err, ErrNoSync) {
				return nil, ErrNoSync
			}
			return nil, err
		}

		data := r.buf[r.start:r.end]
		idx := -1
		if data[0] == SyncByte {
			idx = 0
		} else {
			idx = findSync(data, r.eof || errors.Is(err, io.EOF), true)
		}
		if idx < 0 {
			if r.eof || errors.Is(err, io.EOF) {
				r.stats.GarbageBytes += int64(avail)
				r.start = r.end
				if !r.anyPacket {
					return nil, ErrNoSync
				}
				return nil, io.EOF
			}
			keep := PacketSize
			if keep > avail {
				keep = avail
			}
			discard := avail - keep
			if discard <= 0 {
				discard = avail
			}
			r.stats.GarbageBytes += int64(discard)
			r.stats.SyncResyncs++
			r.start += discard
			continue
		}
		if idx > 0 {
			r.stats.GarbageBytes += int64(idx)
			r.stats.SyncResyncs++
			r.start += idx
			data = r.buf[r.start:r.end]
		}
		if len(data) < PacketSize {
			continue
		}
		r.start += PacketSize
		r.stats.BytesRead += PacketSize
		p := &r.packet
		ok := parse(data[:PacketSize], p)
		if !ok {
			continue
		}
		r.anyPacket = true
		r.stats.Packets++
		r.account(p)
		return p, nil
	}
}

func (r *Reader) fill(n int) (int, error) {
	for {
		avail := r.end - r.start
		if avail >= n || (r.eof && avail > 0) {
			return avail, r.readErr
		}
		if r.eof {
			return avail, io.EOF
		}
		if r.start > 0 {
			copy(r.buf, r.buf[r.start:r.end])
			r.end -= r.start
			r.start = 0
		}
		if r.end == len(r.buf) {
			return r.end - r.start, io.ErrShortBuffer
		}
		nr, err := r.r.Read(r.buf[r.end:])
		if nr > 0 {
			r.end += nr
		}
		if err != nil {
			r.eof = true
			r.readErr = err
			return r.end - r.start, err
		}
		if nr == 0 {
			return r.end - r.start, io.ErrNoProgress
		}
	}
}

func findSync(data []byte, eof, requireNext bool) int {
	for i := 0; i+PacketSize <= len(data); i++ {
		if data[i] != SyncByte {
			continue
		}
		if !requireNext {
			return i
		}
		if i+PacketSize < len(data) {
			if data[i+PacketSize] == SyncByte {
				return i
			}
			continue
		}
		if eof {
			return i
		}
	}
	return -1
}

func parse(raw []byte, p *Packet) bool {
	*p = Packet{
		Raw:           raw,
		PID:           uint16(raw[1]&0x1f)<<8 | uint16(raw[2]),
		PUSI:          raw[1]&0x40 != 0,
		TEI:           raw[1]&0x80 != 0,
		CC:            raw[3] & 0x0f,
		HasAdaptation: raw[3]&0x20 != 0,
		HasPayload:    raw[3]&0x10 != 0,
	}
	payloadAt := headerSize
	if p.HasAdaptation {
		if len(raw) < 5 {
			return false
		}
		afLen := int(raw[4])
		if 5+afLen > len(raw) {
			return false
		}
		payloadAt = 5 + afLen
		if afLen > 0 {
			flags := raw[5]
			p.Discontinuity = flags&0x80 != 0
			p.RandomAccess = flags&0x40 != 0
			p.HasPCR = flags&0x10 != 0
			if p.HasPCR && afLen >= 7 {
				b := raw[6:12]
				p.PCRBase = uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 |
					uint64(b[3])<<1 | uint64(b[4])>>7
			}
		}
	}
	p.AdaptationOnly = p.HasAdaptation && !p.HasPayload
	if p.HasPayload {
		if payloadAt > len(raw) {
			return false
		}
		p.Payload = raw[payloadAt:]
	}
	return true
}

func (r *Reader) account(p *Packet) {
	if p.TEI {
		r.stats.TEIPackets++
	}
	if p.PID == NullPID {
		r.stats.NullPackets++
	}
	if p.AdaptationOnly {
		r.stats.AdaptationOnly++
	}
	if p.PID == NullPID || !p.HasPayload || p.TEI {
		return
	}
	last, ok := r.cc[p.PID]
	if !ok || !last.valid {
		r.cc[p.PID] = ccState{last: p.CC, valid: true}
		return
	}
	if p.Discontinuity {
		r.stats.Discontinuities++
		r.cc[p.PID] = ccState{last: p.CC, valid: true}
		return
	}
	step := (p.CC - last.last + 16) & 0x0f
	switch {
	case step == 0:
		p.Duplicate = true
		r.stats.DuplicatePackets++
	case step == 1:
		r.cc[p.PID] = ccState{last: p.CC, valid: true}
	case step <= r.ccThreshold:
		r.stats.LostPackets += int64(step - 1)
		r.cc[p.PID] = ccState{last: p.CC, valid: true}
	default:
		p.CCJump = true
		r.stats.CCJumps++
		r.stats.CCResyncs++
		r.cc[p.PID] = ccState{last: p.CC, valid: true}
	}
}

func (s Stats) String() string {
	return fmt.Sprintf("packets=%d garbage=%d resync=%d tei=%d adapt_only=%d null=%d dup=%d cc_jumps=%d lost=%d",
		s.Packets, s.GarbageBytes, s.SyncResyncs, s.TEIPackets, s.AdaptationOnly,
		s.NullPackets, s.DuplicatePackets, s.CCJumps, s.LostPackets)
}
