// Package sample deterministically generates the local RTP/RTCP demo
// stream, including packet loss, reordering, late and duplicate
// packets, an RTCP clock jump and signaling conflicts. No network or
// external files are involved.
package sample

import (
	"encoding/binary"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// TrackSpec describes one generated track.
type TrackSpec struct {
	SSRC        uint32
	ClockRate   int64
	Channels    int
	Layout      string
	IsVideo     bool
	PayloadType uint8
	BaseSeq     uint16
	BaseTS      uint32
	FrameTicks  uint32 // RTP timestamp increment per packet
	Frames      int    // audio frames per packet
	PayloadLen  int    // video payload bytes per packet
}

// Specs is the fixed track table of the demo:棚内对白 (48 kHz stereo),
// 外景环境声 (44.1 kHz 5.1) and two 90 kHz video cameras.
func Specs() []TrackSpec {
	return []TrackSpec{
		{SSRC: 1001, ClockRate: 48000, Channels: 2, Layout: "stereo",
			PayloadType: 96, BaseSeq: 65500, BaseTS: 48000, FrameTicks: 960, Frames: 960},
		{SSRC: 1002, ClockRate: 44100, Channels: 6, Layout: "5.1",
			PayloadType: 97, BaseSeq: 7, BaseTS: 44100, FrameTicks: 882, Frames: 882},
		{SSRC: 1003, ClockRate: 90000, IsVideo: true,
			PayloadType: 98, BaseSeq: 100, BaseTS: 90000, FrameTicks: 1800, PayloadLen: 400},
		{SSRC: 1004, ClockRate: 90000, IsVideo: true,
			PayloadType: 99, BaseSeq: 200, BaseTS: 360000, FrameTicks: 1800, PayloadLen: 400},
	}
}

// Item references one packet of the stream by its ideal identity.
type Item struct {
	ID     int64 // position in the ideal (fault-free) stream
	Track  int   // index into Specs()
	N      int64 // per-track packet (or sender report) index
	IsRTCP bool
}

// srEveryTicks is the sender report cadence; srJumpK is the report
// index at which track 2's clock steps by srJumpNanos.
const (
	srEveryTicks = 250
	srJumpTrack  = 2
	srJumpK      = 20
	srJumpNanos  = int64(750_000_000)
	tickNanos    = int64(20_000_000) // 20 ms per tick
	ntpEpoch     = uint64(2208988800)
	ntpBaseSec   = uint64(1767225600) // 2026-01-01T00:00:00Z
)

// BuildIdeal builds the fault-free stream of exactly packets items.
func BuildIdeal(packets int) []Item {
	specs := Specs()
	items := make([]Item, 0, packets)
	n := make([]int64, len(specs))
	sr := make([]int64, len(specs))
	for tick := 0; len(items) < packets; tick++ {
		for tr := range specs {
			if len(items) >= packets {
				break
			}
			items = append(items, Item{Track: tr, N: n[tr]})
			n[tr]++
		}
		if tick%srEveryTicks == 0 {
			for tr := range specs {
				if len(items) >= packets {
					break
				}
				items = append(items, Item{Track: tr, N: sr[tr], IsRTCP: true})
				sr[tr]++
			}
		}
	}
	for i := range items {
		items[i].ID = int64(i)
	}
	return items
}

type rng struct{ s uint64 }

func prng(seed, id int64) rng {
	return rng{s: uint64(seed)*6364136223846793005 + uint64(id)*1442695040888963407 + 0x9e3779b97f4a7c15}
}

func (r *rng) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s * 2685821657736338717
}

// Render regenerates the wire bytes of one item, purely from (seed,
// item), so the stream never has to be held in memory as bytes.
func Render(seed int64, it Item) []byte {
	spec := Specs()[it.Track]
	if it.IsRTCP {
		return renderSR(spec, it.N)
	}
	r := prng(seed, it.ID)
	var payload []byte
	if spec.IsVideo {
		payload = make([]byte, spec.PayloadLen)
		for i := 0; i < len(payload); i += 8 {
			binary.LittleEndian.PutUint64(payload[i:], r.next())
		}
	} else {
		payload = make([]byte, spec.Frames*spec.Channels*2)
		for i := 0; i < len(payload); i += 2 {
			binary.LittleEndian.PutUint16(payload[i:], uint16(r.next()>>48))
		}
	}
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    spec.PayloadType,
			SequenceNumber: spec.BaseSeq + uint16(it.N),
			Timestamp:      spec.BaseTS + uint32(it.N)*spec.FrameTicks,
			SSRC:           spec.SSRC,
		},
		Payload: payload,
	}
	raw, err := pkt.Marshal()
	if err != nil {
		panic(err)
	}
	return raw
}

func renderSR(spec TrackSpec, k int64) []byte {
	nanos := int64(ntpBaseSec) * 1_000_000_000
	nanos += k * srEveryTicks * tickNanos
	if spec.SSRC == Specs()[srJumpTrack].SSRC && k >= srJumpK {
		nanos += srJumpNanos // permanent clock step
	}
	sec := uint64(nanos/1_000_000_000) + ntpEpoch
	frac := uint64(nanos%1_000_000_000) * (1 << 32) / 1_000_000_000
	sr := &rtcp.SenderReport{
		SSRC:        spec.SSRC,
		NTPTime:     sec<<32 | frac,
		RTPTime:     spec.BaseTS + uint32(k)*srEveryTicks*spec.FrameTicks,
		PacketCount: uint32(k * srEveryTicks),
		OctetCount:  uint32(k * srEveryTicks * 1024),
	}
	raw, err := sr.Marshal()
	if err != nil {
		panic(err)
	}
	return raw
}
