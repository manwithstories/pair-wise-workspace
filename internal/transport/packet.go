// Package transport parses MPEG-TS packets and validates PSI tables.
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	PacketSize = 188
	SyncByte   = 0x47
)

// Packet is a parsed fixed-size MPEG-TS packet.
type Packet struct {
	Offset        int64
	PID           uint16
	PUSI          bool
	Priority      bool
	Scrambling    uint8
	AdaptationAFC uint8
	CC            uint8
	PCR           uint64
	HasPCR        bool
	Payload       []byte
}

// CCGap describes a continuity-counter discontinuity. Missing is the forward
// CC distance that was not observed; no media bytes are fabricated.
type CCGap struct {
	PID      uint16
	Offset   int64
	Expected uint8
	Got      uint8
	Missing  int
}

// TruncatedError reports a partial final TS packet.
type TruncatedError struct {
	Got  int
	Want int
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf("truncated TS packet: got %d bytes, want %d", e.Got, e.Want)
}

// ParsePacket parses one 188-byte packet. The returned payload is an
// independent copy so callers may retain packets from a reused read buffer.
func ParsePacket(raw []byte, offset int64) (Packet, error) {
	if len(raw) != PacketSize {
		return Packet{}, fmt.Errorf("invalid TS packet length %d", len(raw))
	}
	if raw[0] != SyncByte {
		return Packet{}, fmt.Errorf("invalid sync byte 0x%02x at offset %d", raw[0], offset)
	}

	p := Packet{
		Offset:        offset,
		PUSI:          raw[1]&0x40 != 0,
		Priority:      raw[1]&0x20 != 0,
		PID:           uint16(raw[1]&0x1f)<<8 | uint16(raw[2]),
		Scrambling:    raw[3] >> 6,
		AdaptationAFC: (raw[3] >> 4) & 0x03,
		CC:            raw[3] & 0x0f,
	}

	payloadStart := 4
	switch p.AdaptationAFC {
	case 0:
		return p, errors.New("reserved adaptation_field_control=00")
	case 2, 3:
		al := int(raw[4])
		if al > PacketSize-5 {
			return p, fmt.Errorf("adaptation field length %d too large", al)
		}
		if al >= 1 && raw[5]&0x10 != 0 {
			if al < 7 {
				return p, errors.New("PCR flag set but adaptation field is too short")
			}
			b := raw[6:12]
			p.PCR = uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 |
				uint64(b[3])<<1 | uint64(b[4])>>7
			p.HasPCR = true
		}
		payloadStart = 5 + al
	}

	if p.AdaptationAFC == 1 || p.AdaptationAFC == 3 {
		if payloadStart > PacketSize {
			return p, errors.New("payload starts beyond packet")
		}
		p.Payload = append([]byte(nil), raw[payloadStart:]...)
	}
	return p, nil
}

// Reader reads and optionally checks CC sequences from an io.Reader.
type Reader struct {
	r       io.Reader
	offset  int64
	checkCC bool
	lastCC  map[uint16]uint8
	seenPID map[uint16]bool
	CCGaps  []CCGap
	mu      sync.Mutex
}

// NewReader creates a TS packet reader.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r, lastCC: make(map[uint16]uint8), seenPID: make(map[uint16]bool)}
}

// NewLimitedReader bounds the input to maxBytes, supporting bounded ingestion.
func NewLimitedReader(r io.Reader, maxBytes int64) *Reader {
	return NewReader(io.LimitReader(r, maxBytes))
}

// WithCCCheck enables continuity-counter gap detection.
func (r *Reader) WithCCCheck() *Reader {
	r.checkCC = true
	return r
}

// Read reads exactly one TS packet.
func (r *Reader) Read() (Packet, error) {
	var raw [PacketSize]byte
	n, err := io.ReadFull(r.r, raw[:])
	if errors.Is(err, io.EOF) && n == 0 {
		return Packet{}, io.EOF
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || (err == nil && n < PacketSize) {
		return Packet{}, &TruncatedError{Got: n, Want: PacketSize}
	}
	if err != nil {
		return Packet{}, err
	}

	start := r.offset
	r.offset += PacketSize
	p, err := ParsePacket(raw[:], start)
	if err != nil {
		return p, err
	}
	if r.checkCC && p.PID != 0x1fff {
		r.checkContinuity(p)
	}
	return p, nil
}

func (r *Reader) checkContinuity(p Packet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.seenPID[p.PID] {
		r.seenPID[p.PID] = true
		r.lastCC[p.PID] = p.CC
		return
	}
	last := r.lastCC[p.PID]
	expected := (last + 1) & 0x0f
	// An identical CC is a legal duplicate; do not classify it as a jump.
	if p.CC != expected && p.CC != last {
		missing := (int(p.CC) - int(expected) + 16) % 16
		if missing == 0 {
			missing = 16
		}
		gap := CCGap{PID: p.PID, Offset: p.Offset, Expected: expected, Got: p.CC, Missing: missing}
		r.CCGaps = append(r.CCGaps, gap)
	}
	r.lastCC[p.PID] = p.CC
}

// Gaps returns a copy of detected CC gaps.
func (r *Reader) Gaps() []CCGap {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CCGap(nil), r.CCGaps...)
}

// Queue is a bounded TS packet queue. A full channel blocks the producer,
// providing backpressure instead of dropping media or growing without limit.
type Queue struct {
	ch            chan Packet
	consumerDelay time.Duration
	highWater     int
	mu            sync.Mutex
}

// NewQueue creates a queue with a fixed packet capacity.
func NewQueue(capacity int, consumerDelay time.Duration) *Queue {
	if capacity < 1 {
		capacity = 1
	}
	return &Queue{ch: make(chan Packet, capacity), consumerDelay: consumerDelay}
}

// Push blocks on backpressure and exits promptly when ctx is cancelled.
func (q *Queue) Push(ctx context.Context, p Packet) error {
	select {
	case q.ch <- p:
		q.mu.Lock()
		if n := len(q.ch); n > q.highWater {
			q.highWater = n
		}
		q.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pop applies the configured consumer work delay while remaining cancellable.
func (q *Queue) Pop(ctx context.Context) (Packet, error) {
	if q.consumerDelay > 0 {
		t := time.NewTimer(q.consumerDelay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return Packet{}, ctx.Err()
		}
	}
	select {
	case p := <-q.ch:
		return p, nil
	case <-ctx.Done():
		return Packet{}, ctx.Err()
	}
}

// Close closes the underlying channel.
func (q *Queue) Close() { close(q.ch) }

// HighWater returns the largest observed queue length.
func (q *Queue) HighWater() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.highWater
}

// Cap returns queue capacity.
func (q *Queue) Cap() int { return cap(q.ch) }
