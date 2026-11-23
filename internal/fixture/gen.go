// Package fixture generates self-contained RTP event streams with
// controlled impairments: loss bursts, reordering, duplicates and tail
// truncation. All corruption, loss and boundary fixtures are produced
// here in Go code; no external samples are referenced.
package fixture

import (
	"fmt"
	"math/rand"
	"time"

	pionrtp "github.com/pion/rtp"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
)

// SSRC is the fixed stream identifier used by generated fixtures.
const SSRC uint32 = 0xA4C11E01

// Params describes one generated stream.
type Params struct {
	Codec       string // "opus" | "aac"
	Duration    time.Duration
	Seed        int64
	LossBurst   int // packets dropped per burst
	Reorder     int // packets delayed by one position
	Duplicate   int // packets emitted twice
	CorruptTail int // tail packets truncated after emission
	TargetKbps  float64
	Channels    uint8
}

// Stream is a generated wire-format RTP stream in arrival order.
type Stream struct {
	Config   event.CodecConfig
	Raw      [][]byte
	Arrival  []clock.Time
	Expected int // source packets before impairments
}

// packetTicks returns the codec-clock ticks per packet.
func (p Params) packetTicks() (uint64, uint32, error) {
	switch p.Codec {
	case "opus":
		return 960, 48000, nil // 20 ms at 48 kHz
	case "aac":
		return 1024, 48000, nil // one AAC-LC frame at 48 kHz
	}
	return 0, 0, fmt.Errorf("unsupported codec %q", p.Codec)
}

// Config returns the codec configuration for the parameters.
func (p Params) Config() (event.CodecConfig, error) {
	ticks, rate, err := p.packetTicks()
	_ = ticks
	if err != nil {
		return event.CodecConfig{}, err
	}
	ch := p.Channels
	if ch == 0 {
		ch = 2
	}
	cfg := event.CodecConfig{ClockRate: rate, Channels: ch}
	switch p.Codec {
	case "opus":
		cfg.Codec = event.CodecOpus
	case "aac":
		cfg.Codec = event.CodecAAC
	}
	return cfg, nil
}

// Generate builds the impaired stream deterministically from p.Seed.
// Sequence numbers and RTP timestamps start near their wrap points so the
// generated stream always exercises wrap-around handling.
func Generate(p Params) (*Stream, error) {
	cfg, err := p.Config()
	if err != nil {
		return nil, err
	}
	ticks, _, _ := p.packetTicks()
	interval := time.Duration(ticks) * time.Second / time.Duration(cfg.ClockRate)
	count := int(p.Duration / interval)
	if count < 16 {
		return nil, fmt.Errorf("duration %s too short: %d packets", p.Duration, count)
	}
	rng := rand.New(rand.NewSource(p.Seed))
	payloadLen := int(p.TargetKbps * 1000 / 8 * interval.Seconds())
	if payloadLen < 4 {
		payloadLen = 4
	}

	// Source packets.
	type srcPkt struct {
		raw []byte
	}
	src := make([]srcPkt, count)
	var seq uint16 = 65000
	var ts uint32 = 0xFFFFFF00
	for i := 0; i < count; i++ {
		payload := make([]byte, payloadLen)
		switch cfg.Codec {
		case event.CodecOpus:
			payload[0] = 19<<3 | 0x4 // CELT 20 ms, stereo, code 0
			rng.Read(payload[1:])
		case event.CodecAAC:
			// AU-headers-length = 0, then AU data.
			rng.Read(payload[2:])
		}
		pkt := pionrtp.Packet{
			Header: pionrtp.Header{
				Version: 2, SSRC: SSRC,
				SequenceNumber: seq, Timestamp: ts,
			},
			Payload: payload,
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, err
		}
		src[i] = srcPkt{raw: raw}
		seq++
		ts += uint32(ticks)
	}

	// Loss bursts: two deterministic burst positions with seed jitter.
	lost := make(map[int]bool)
	if p.LossBurst > 0 {
		bursts := 2
		if count < 200 {
			bursts = 1
		}
		for b := 0; b < bursts; b++ {
			at := count*(2*b+1)/(2*bursts) + rng.Intn(11) - 5
			for k := 0; k < p.LossBurst && at+k < count-1; k++ {
				lost[at+k] = true
			}
		}
	}

	// Surviving packets in source order.
	var order []int
	for i := 0; i < count; i++ {
		if !lost[i] {
			order = append(order, i)
		}
	}

	// Reorder: swap a packet with its successor.
	for k := 0; k < p.Reorder && len(order) > 2; k++ {
		at := 1 + rng.Intn(len(order)-2)
		order[at], order[at+1] = order[at+1], order[at]
	}

	// Emit, duplicating selected packets.
	var stream Stream
	stream.Config = cfg
	stream.Expected = count
	dupAt := make(map[int]bool)
	for k := 0; k < p.Duplicate && len(order) > 0; k++ {
		dupAt[rng.Intn(len(order))] = true
	}
	arrival := clock.Time(0)
	for i, idx := range order {
		stream.Raw = append(stream.Raw, src[idx].raw)
		arrival += clock.FromDuration(time.Millisecond)
		stream.Arrival = append(stream.Arrival, arrival)
		if dupAt[i] {
			stream.Raw = append(stream.Raw, src[idx].raw)
			arrival += clock.FromDuration(time.Millisecond)
			stream.Arrival = append(stream.Arrival, arrival)
		}
	}

	// Corrupt tail: truncate the last datagrams, keeping the RTP header
	// intact but cutting the payload short.
	for k := 0; k < p.CorruptTail && k < len(stream.Raw); k++ {
		i := len(stream.Raw) - 1 - k
		if n := 13; n < len(stream.Raw[i]) {
			stream.Raw[i] = stream.Raw[i][:n]
		}
	}
	return &stream, nil
}
