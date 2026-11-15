// Package media provides deterministic test media without microphone access.
package media

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/pion/rtp"
)

const (
	DefaultClockRate     = 48000
	DefaultPayloadType   = 111
	DefaultFrameDuration = 20 * time.Millisecond
)

// Config controls the synthetic audio-like RTP source.
type Config struct {
	SSRC          uint32
	PayloadType   uint8
	ClockRate     uint32
	FrameDuration time.Duration
	Seed          uint64
	PayloadSize   int
}

// Source emits one deterministic RTP packet per call to Next.
type Source struct {
	cfg   Config
	state uint64
	seq   uint16
	ts    uint32
}

// NewSource builds a source with sensible Opus-like defaults.
func NewSource(cfg Config) *Source {
	if cfg.PayloadType == 0 {
		cfg.PayloadType = DefaultPayloadType
	}
	if cfg.ClockRate == 0 {
		cfg.ClockRate = DefaultClockRate
	}
	if cfg.FrameDuration <= 0 {
		cfg.FrameDuration = DefaultFrameDuration
	}
	if cfg.PayloadSize <= 0 {
		cfg.PayloadSize = 160
	}
	if cfg.Seed == 0 {
		cfg.Seed = 1
	}
	return &Source{cfg: cfg, state: cfg.Seed}
}

// Next returns the next 20ms frame. Sequence and timestamp wrap naturally.
func (s *Source) Next() *rtp.Packet {
	payload := make([]byte, s.cfg.PayloadSize)
	for i := range payload {
		s.state ^= s.state << 13
		s.state ^= s.state >> 7
		s.state ^= s.state << 17
		payload[i] = byte(s.state)
	}
	packet := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    s.cfg.PayloadType,
			SequenceNumber: s.seq,
			Timestamp:      s.ts,
			SSRC:           s.cfg.SSRC,
		},
		Payload: payload,
	}
	s.seq++
	s.ts += uint32(s.cfg.ClockRate * uint32(s.cfg.FrameDuration/time.Millisecond) / 1000)
	return packet
}

// Config returns the immutable source configuration.
func (s *Source) Config() Config { return s.cfg }

// Run emits frames on the configured cadence until ctx is canceled.
func (s *Source) Run(ctx context.Context, write func(*rtp.Packet) error) error {
	ticker := time.NewTicker(s.cfg.FrameDuration)
	defer ticker.Stop()
	for {
		if err := write(s.Next()); err != nil {
			return fmt.Errorf("write source packet: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunCount emits exactly count frames on the frame cadence. It is the
// reproducible path used by verify and the manual publisher.
func (s *Source) RunCount(ctx context.Context, count int, write func(*rtp.Packet) error) error {
	if count <= 0 {
		return nil
	}
	ticker := time.NewTicker(s.cfg.FrameDuration)
	defer ticker.Stop()
	for i := 0; i < count; i++ {
		if err := write(s.Next()); err != nil {
			return fmt.Errorf("write source packet: %w", err)
		}
		if i == count-1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	return nil
}

// PacketCount returns how many packets fit in duration at the source cadence.
func PacketCount(duration time.Duration, frame time.Duration) int {
	if frame <= 0 {
		return 0
	}
	return int(duration / frame)
}

// EncodePayloadSeed is useful for deterministic fixtures in tests.
func EncodePayloadSeed(payload []byte) uint64 {
	if len(payload) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(payload[:8])
}
