// Package ingest turns RTP packets carrying 16-bit interleaved PCM into
// clock.Block values positioned on the unified sample timeline.
package ingest

import (
	"github.com/pion/rtp"

	"avcheck/internal/clock"
)

// Packetizer builds RTP packets from sample frames. It is used by the local
// fixture generator; production input uses Parse.
type Packetizer struct {
	SSRC         uint32
	TrackID      uint32
	Channels     int
	FramesPerPkt int
	clockRate    uint32
	seq          uint16
	ts           uint32
}

// NewPacketizer creates a packetizer on the unified clock.
func NewPacketizer(ssrc, track uint32, channels, framesPerPkt int) *Packetizer {
	return &Packetizer{
		SSRC:         ssrc,
		TrackID:      track,
		Channels:     channels,
		FramesPerPkt: framesPerPkt,
		clockRate:    clock.SampleRate,
	}
}

// Packetize wraps one run of interleaved int32 frames into an RTP packet.
// Samples are converted back to 16-bit PCM; frames must be FramesPerPkt.
func (p *Packetizer) Packetize(frames []int32) (*rtp.Packet, error) {
	if len(frames) != p.FramesPerPkt*p.Channels {
		return nil, clock.NewError(clock.KindBadBlock,
			"frame count %d not a multiple of %d channels x %d frames",
			len(frames), p.Channels, p.FramesPerPkt)
	}
	payload := make([]byte, len(frames)*2)
	for i, s := range frames {
		v := uint16(int16(s))
		payload[i*2] = byte(v)
		payload[i*2+1] = byte(v >> 8)
	}
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SSRC:           p.SSRC,
			SequenceNumber: p.seq,
			Timestamp:      p.ts,
		},
		Payload: payload,
	}
	p.seq++
	p.ts += uint32(p.FramesPerPkt)
	return pkt, nil
}

// Parser converts raw RTP datagrams to clock.Block values. It keeps per-SSRC
// extended RTP timestamps so 32-bit wrap stays monotonic on the 64-bit
// unified timeline.
type Parser struct {
	channels int
	trackOf  map[uint32]uint32
	lastTS   map[uint32]uint32
	extTS    map[uint32]int64
}

// NewParser creates a parser. channels is the fixed interleaving of the stream.
func NewParser(channels int) *Parser {
	if channels < 1 || channels > 8 {
		return nil
	}
	return &Parser{
		channels: channels,
		trackOf:  make(map[uint32]uint32),
		lastTS:   make(map[uint32]uint32),
		extTS:    make(map[uint32]int64),
	}
}

// BindTrack declares which logical track an SSRC belongs to.
func (p *Parser) BindTrack(ssrc, track uint32) {
	p.trackOf[ssrc] = track
}

// Parse decodes one raw RTP datagram. ArrivalMS is the virtual receive time
// in milliseconds, propagated onto the block for latency bookkeeping.
func (p *Parser) Parse(raw []byte, arrivalMS int64) (*clock.Block, error) {
	if len(raw) > clock.MaxBlockBytes+72 { // header overhead allowance
		return nil, clock.NewError(clock.KindBadBlock,
			"datagram %d bytes exceeds %d byte cap", len(raw), clock.MaxBlockBytes)
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(raw); err != nil {
		return nil, clock.NewError(clock.KindBadBlock, "rtp unmarshal: %v", err)
	}
	if len(pkt.Payload) == 0 || len(pkt.Payload)%(2*p.channels) != 0 {
		return nil, clock.NewError(clock.KindBadBlock,
			"ssrc=%d payload %d bytes not %d-channel 16-bit aligned",
			pkt.SSRC, len(pkt.Payload), p.channels)
	}
	if len(pkt.Payload) > clock.MaxBlockBytes {
		return nil, clock.NewError(clock.KindBadBlock,
			"payload %d bytes exceeds %d byte block cap",
			len(pkt.Payload), clock.MaxBlockBytes)
	}
	start := p.extendTimestamp(pkt.SSRC, pkt.Timestamp)
	frames := len(pkt.Payload) / (2 * p.channels)
	samples := make([]int32, frames*p.channels)
	for i := range samples {
		v := int16(pkt.Payload[i*2]) | int16(pkt.Payload[i*2+1])<<8
		samples[i] = int32(v)
	}
	track, ok := p.trackOf[pkt.SSRC]
	if !ok {
		track = uint32(pkt.SSRC)
	}
	return &clock.Block{
		TrackID:   track,
		SSRC:      pkt.SSRC,
		Seq:       pkt.SequenceNumber,
		StartTick: start,
		Channels:  p.channels,
		Samples:   samples,
		Source:    clock.SourceOriginal,
		ArrivalMS: arrivalMS,
	}, nil
}

// extendTimestamp maps a wrapping 32-bit RTP timestamp onto a monotonic
// int64 tick on the unified timeline.
func (p *Parser) extendTimestamp(ssrc uint32, ts uint32) int64 {
	last, seen := p.lastTS[ssrc]
	base := p.extTS[ssrc]
	if !seen {
		p.lastTS[ssrc] = ts
		p.extTS[ssrc] = int64(ts)
		return int64(ts)
	}
	delta := int32(ts - last) // unsigned subtraction yields signed wrap delta
	ext := base + int64(delta)
	p.lastTS[ssrc] = ts
	p.extTS[ssrc] = ext
	return ext
}
