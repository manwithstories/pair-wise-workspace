// Package rtp maps rational media timestamps onto RTP header timestamps and
// validates the resulting packet stream with pion/rtp. All conversions stay
// integer/rational so repeated runs and clock-rate changes never drift.
package rtp

import (
	"sync"

	"github.com/pion/rtp"

	"medpipe/internal/clock"
)

// Clock rates used by the media profiles this service emits.
const (
	AudioClockRate = 48000
	VideoClockRate = 90000
)

// Mapper converts media ticks on a track timescale into RTP timestamps on a
// fixed RTP clock rate, handling the uint32 wraparound at 2^32.
type Mapper struct {
	clockRate   uint32
	timescale   uint32
	baseMedia   uint64 // first media tick treated as RTP time zero
	initialized bool
	mu          sync.Mutex
}

// NewMapper builds a mapper from a track timescale to an RTP clock rate.
func NewMapper(timescale, clockRate uint32) (*Mapper, error) {
	if timescale == 0 || clockRate == 0 {
		return nil, clock.E(clock.KindArgument, "rtp.map",
			clock.WithDetail("timescale %d and clock rate %d must be non-zero",
				timescale, clockRate))
	}
	return &Mapper{clockRate: clockRate, timescale: timescale}, nil
}

// ToRTP converts one media tick to its RTP timestamp. The first observed
// tick establishes the origin so streams starting at non-zero media time
// still begin near RTP timestamp zero.
func (m *Mapper) ToRTP(tick clock.Tick) (uint32, error) {
	if tick.Timescale != m.timescale {
		return 0, clock.E(clock.KindArgument, "rtp.map",
			clock.WithDetail("tick timescale %d != mapper %d",
				tick.Timescale, m.timescale))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.initialized {
		m.baseMedia = tick.Value
		m.initialized = true
	}
	if tick.Value < m.baseMedia {
		return 0, clock.E(clock.KindCorrupt, "rtp.map", clock.WithTick(tick),
			clock.WithDetail("media timestamp before stream origin"))
	}
	relative := tick.Value - m.baseMedia
	// Exact rational: rtp = relative * clockRate / timescale, rounded.
	scaled := relative*uint64(m.clockRate) + uint64(m.timescale)/2
	abs := scaled / uint64(m.timescale)
	return uint32(abs), nil
}

// Packetizer emits validated RTP packets carrying media timestamps. It keeps
// sequence numbers and detects non-monotonic timestamps after wraparound.
type Packetizer struct {
	mapper *Mapper
	ssrc   uint32
	pt     uint8
	mu     sync.Mutex
	seq    uint16
	// lastAbs stores the unwrapped 64-bit RTP time for monotonic checks.
	lastAbs uint64
	started bool
}

// NewPacketizer builds a packetizer over an existing mapper.
func NewPacketizer(mapper *Mapper, ssrc uint32, payloadType uint8) *Packetizer {
	return &Packetizer{mapper: mapper, ssrc: ssrc, pt: payloadType}
}

// Packet builds one RTP packet for a media tick. A timestamp that moves
// backwards (after accounting for uint32 wrap) is rejected.
func (p *Packetizer) Packet(tick clock.Tick, payload []byte, marker bool) (*rtp.Packet, error) {
	ts32, err := p.mapper.ToRTP(tick)
	if err != nil {
		return nil, err
	}
	// Recover the absolute (unwrapped) value to test true monotonicity.
	p.mu.Lock()
	abs := p.unwrap(ts32)
	if p.started && abs < p.lastAbs {
		p.mu.Unlock()
		return nil, clock.E(clock.KindCorrupt, "rtp.packet", clock.WithTick(tick),
			clock.WithDetail("RTP timestamp moved backwards: %d < %d", abs, p.lastAbs))
	}
	p.started = true
	p.lastAbs = abs
	seq := p.seq
	p.seq++
	p.mu.Unlock()

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    p.pt,
			SequenceNumber: seq,
			Timestamp:      ts32,
			SSRC:           p.ssrc,
			Marker:         marker,
		},
		Payload: payload,
	}
	// Round-trip through pion to prove the header is wire-valid.
	raw, err := pkt.Marshal()
	if err != nil {
		return nil, clock.E(clock.KindOther, "rtp.packet", clock.WithWrap(err))
	}
	var verified rtp.Packet
	if err := verified.Unmarshal(raw); err != nil {
		return nil, clock.E(clock.KindCorrupt, "rtp.packet",
			clock.WithDetail("pion rejected packet"), clock.WithWrap(err))
	}
	if verified.Timestamp != ts32 || verified.SequenceNumber != seq {
		return nil, clock.E(clock.KindCorrupt, "rtp.packet",
			clock.WithDetail("round-trip header mismatch"))
	}
	return pkt, nil
}

// unwrap extends a 32-bit timestamp to 64 bits using the previous absolute
// value, tolerating exactly one forward wraparound.
func (p *Packetizer) unwrap(ts uint32) uint64 {
	if !p.started {
		return uint64(ts)
	}
	low := p.lastAbs & 0xffffffff
	base := p.lastAbs - low
	switch {
	case uint64(ts)+(1<<31) < low:
		// ts wrapped forward into the next cycle.
		return base + (1 << 32) + uint64(ts)
	case uint64(ts) > low+(1<<31):
		// ts wrapped backward (rare, large negative jump).
		if base >= (1 << 32) {
			return base - (1 << 32) + uint64(ts)
		}
		return uint64(ts)
	default:
		return base + uint64(ts)
	}
}
