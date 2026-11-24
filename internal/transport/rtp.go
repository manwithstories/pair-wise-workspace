// Package transport wraps samples in RTP and recovers packet loss using
// sequence and timestamp bookkeeping. It owns no media knowledge beyond
// "these bytes belong to this track at this media time"; all timing is passed
// in as unified-clock microseconds.
package transport

import (
	"fmt"

	"github.com/pion/rtp"
)

// PacketConfig controls RTP packaging.
type PacketConfig struct {
	PayloadType  uint8
	SSRC         uint32
	MTU          int
	ClockRate    uint32
	InitialSeq   uint16
	InitialTs    uint32
	MarkerOnLast bool
}

// DefaultPacketConfig is a 90kHz video RTP profile with a 1200-byte MTU.
func DefaultPacketConfig() PacketConfig {
	return PacketConfig{
		PayloadType:  96,
		SSRC:         0x52455031,
		MTU:          1200,
		ClockRate:    90000,
		MarkerOnLast: true,
	}
}

// Envelope is one RTP packet plus the sample coordinates needed to regroup.
type Envelope struct {
	Packet      *rtp.Packet
	TrackID     uint32
	SampleIndex int
	FragIndex   int
	FragCount   int
	MediaUs     int64
	DurationUs  int64
}

// SentSample describes a sample handed to the packager.
type SentSample struct {
	TrackID    uint32
	Index      int
	MediaUs    int64
	DurationUs int64
	Payload    []byte
}

// Packager turns samples into RTP packets with strictly increasing sequence
// numbers. The RTP timestamp is derived directly from unified media time, so
// it cannot drift with packetization choices.
type Packager struct {
	cfg        PacketConfig
	seq        uint16
	baseUs     int64
	haveBase   bool
	lastPacket int
}

// NewPackager returns a packager with the configured starting sequence.
func NewPackager(cfg PacketConfig) *Packager {
	if cfg.MTU < 64 {
		cfg.MTU = 1200
	}
	if cfg.ClockRate == 0 {
		cfg.ClockRate = 90000
	}
	return &Packager{cfg: cfg, seq: cfg.InitialSeq}
}

// NextSeq returns the sequence number the next packet will use.
func (p *Packager) NextSeq() uint16 { return p.seq }

// Pack slices payload into MTU-sized RTP packets. Every fragment of one
// sample shares a timestamp; the marker bit is set on the last fragment.
func (p *Packager) Pack(s SentSample) ([]Envelope, error) {
	if s.TrackID == 0 {
		return nil, fmt.Errorf("transport: sample has no track id")
	}
	if len(s.Payload) == 0 {
		return nil, fmt.Errorf("transport: sample %d has empty payload", s.Index)
	}
	maxFrag := p.cfg.MTU - 12
	if maxFrag < 1 {
		maxFrag = 1
	}
	if !p.haveBase {
		p.baseUs = s.MediaUs
		p.haveBase = true
	}
	fragCount := (len(s.Payload) + maxFrag - 1) / maxFrag
	ts := p.timestampFor(s.MediaUs)
	envs := make([]Envelope, 0, fragCount)
	for i := 0; i < fragCount; i++ {
		lo := i * maxFrag
		hi := lo + maxFrag
		if hi > len(s.Payload) {
			hi = len(s.Payload)
		}
		payload := make([]byte, hi-lo)
		copy(payload, s.Payload[lo:hi])
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    p.cfg.PayloadType,
				SequenceNumber: p.seq,
				Timestamp:      ts,
				SSRC:           p.cfg.SSRC,
				Marker:         p.cfg.MarkerOnLast && i == fragCount-1,
			},
			Payload: payload,
		}
		envs = append(envs, Envelope{
			Packet: pkt, TrackID: s.TrackID, SampleIndex: s.Index,
			FragIndex: i, FragCount: fragCount, MediaUs: s.MediaUs, DurationUs: s.DurationUs,
		})
		p.seq++
		p.lastPacket++
	}
	return envs, nil
}

func (p *Packager) timestampFor(mediaUs int64) uint32 {
	deltaUs := mediaUs - p.baseUs
	ticks := int64(p.cfg.ClockRate) * deltaUs / 1_000_000
	return p.cfg.InitialTs + uint32(ticks)
}

// PacketsSent returns how many RTP packets were emitted.
func (p *Packager) PacketsSent() int { return p.lastPacket }
