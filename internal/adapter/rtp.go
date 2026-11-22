package adapter

import (
	"fmt"

	"github.com/pion/rtp"

	"mediaroute/internal/clock"
	"mediaroute/internal/fault"
)

// RTP payload convention for the synthetic pipeline: payload byte 0 bit 0
// marks a keyframe. Real depacketization is out of scope.
const rtpFlagKeyframe = 0x1

// RTPParser adapts RTP packets to unified media events. It keeps one
// WrapTracker per track so 32-bit timestamp wraparound is handled per
// track clock. originTicks is the shared stream origin: converted
// timestamps are relative to it, so all tracks land on one timeline.
type RTPParser struct {
	rate        uint32
	originTicks int64
	clocks      map[string]*clock.WrapTracker
}

// NewRTPParser returns a parser for the given sample clock rate (e.g.
// 90000 for video) and stream origin in ticks.
func NewRTPParser(rate uint32, originTicks int64) *RTPParser {
	return &RTPParser{rate: rate, originTicks: originTicks, clocks: map[string]*clock.WrapTracker{}}
}

// MajorVersion implements Parser.
func (p *RTPParser) MajorVersion() int { return 1 }

// Parse unmarshals one RTP packet and projects it onto the unified
// timeline. Timing faults carry track, sequence number and raw timestamp.
func (p *RTPParser) Parse(track string, data []byte, offset int64) ([]Event, error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(data); err != nil {
		return nil, fault.Newf(fault.CatParse, "rtp unmarshal: %v", err).
			WithTrack(track).WithOffset(offset)
	}
	wt, ok := p.clocks[track]
	if !ok {
		if len(p.clocks) >= MaxTracks {
			return nil, fault.Newf(fault.CatParse, "track limit %d exceeded", MaxTracks).
				WithTrack(track).WithOffset(offset)
		}
		wt = clock.NewWrapTracker(p.rate)
		p.clocks[track] = wt
	}
	ext, err := wt.ConvertTicks(pkt.Timestamp)
	if err != nil {
		return nil, fault.New(fault.CatTiming, err.Error()).
			WithTrack(track).WithSeq(uint32(pkt.SequenceNumber)).
			WithRawTime(int64(pkt.Timestamp)).WithOffset(offset)
	}
	ts, err := clock.Scale(ext-p.originTicks, p.rate)
	if err != nil {
		return nil, fault.New(fault.CatTiming, err.Error()).
			WithTrack(track).WithSeq(uint32(pkt.SequenceNumber)).
			WithRawTime(int64(pkt.Timestamp)).WithOffset(offset)
	}
	keyframe := len(pkt.Payload) > 0 && pkt.Payload[0]&rtpFlagKeyframe != 0
	return []Event{{
		Track:       track,
		Kind:        KindMedia,
		Seq:         uint32(pkt.SequenceNumber),
		TimestampNs: ts,
		Keyframe:    keyframe,
		Rendition:   track,
		Payload:     pkt.Payload,
	}}, nil
}

// Snapshot returns per-track wrap state for checkpointing.
func (p *RTPParser) Snapshot() map[string][4]int64 {
	out := make(map[string][4]int64, len(p.clocks))
	for track, wt := range p.clocks {
		e, l, le, init := wt.Snapshot()
		var b int64
		if init {
			b = 1
		}
		out[track] = [4]int64{e, int64(l), le, b}
	}
	return out
}

// Restore reinstates per-track wrap state from a checkpoint.
func (p *RTPParser) Restore(s map[string][4]int64) {
	p.clocks = make(map[string]*clock.WrapTracker, len(s))
	for track, v := range s {
		wt := clock.NewWrapTracker(p.rate)
		wt.Restore(v[0], uint32(v[1]), v[2], v[3] != 0)
		p.clocks[track] = wt
	}
}

// MarshalPacket builds a synthetic RTP packet; used by the simulator.
func MarshalPacket(seq uint16, ts uint32, marker bool, payload []byte) ([]byte, error) {
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, Marker: marker, SSRC: 1},
		Payload: payload,
	}
	buf, err := pkt.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal rtp: %w", err)
	}
	return buf, nil
}
