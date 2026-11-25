package fixture

import (
	"encoding/json"
	"fmt"
	"math/rand"

	"github.com/pion/rtp"

	"fieldqc/internal/audio"
	"fieldqc/internal/color"
	"fieldqc/internal/manifest"
)

// ManifestJSON builds a deterministic manifest for tracks tracks (half
// video, half audio) covering hours hours. badEdit injects an
// out-of-range edit-list entry.
func ManifestJSON(seed int64, tracks, hours int, badEdit bool) []byte {
	rng := rand.New(rand.NewSource(seed))
	durV := int64(hours) * 3600 * 90000
	durA := int64(hours) * 3600 * 48000
	asset := manifest.Asset{ID: "card0"}
	nV := tracks / 2
	for i := 0; i < nV; i++ {
		asset.Tracks = append(asset.Tracks, manifest.Track{
			ID: fmt.Sprintf("cam%c-v", 'a'+i), Kind: "video",
			Timescale: 90000, Duration: durV, StartDTS: int64(rng.Intn(9000)),
			Color: &manifest.ColorMeta{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
		})
	}
	for i := 0; i < tracks-nV; i++ {
		asset.Tracks = append(asset.Tracks, manifest.Track{
			ID: fmt.Sprintf("cam%c-a", 'a'+i), Kind: "audio",
			Timescale: 48000, Duration: durA, StartDTS: int64(rng.Intn(4800)),
			Channels: audio.Layout51, SampleRate: 44100,
		})
	}
	asset.Edits = []manifest.Edit{{MediaTime: 0, Duration: durV, Rate: 1}}
	if badEdit {
		asset.Edits = append(asset.Edits, manifest.Edit{
			MediaTime: durV - 90000, Duration: 3600 * 90000, Rate: 1, // ends 1 h past the media
		})
	}
	for k := int64(0); k < durV; k += 2 * 90000 { // sparse keyframes every 2 s
		asset.Keyframes = append(asset.Keyframes, k)
	}
	b, _ := json.Marshal(manifest.Manifest{Assets: []manifest.Asset{asset}})
	return b
}

// RTPGen synthesizes one RTP stream with random loss, retransmission
// and optional burst loss, in transmit order.
type RTPGen struct {
	rng        *rand.Rand
	seq        uint16
	ts         uint32
	ticks      uint32
	idx        int
	Loss       float64
	Retransmit float64
	BurstAt    int // absolute packet index where a burst starts; <0 off
	BurstLen   int
	queue      []retransmit
}

type retransmit struct {
	pkt *rtp.Packet
	at  int
}

// NewRTPGen creates a generator; ticks is the RTP timestamp step per
// packet (e.g. 3600 for 25 fps at 90 kHz).
func NewRTPGen(seed int64, ticksPerPacket uint32, loss, retransmit float64) *RTPGen {
	return &RTPGen{
		rng: rand.New(rand.NewSource(seed)), ticks: ticksPerPacket,
		Loss: loss, Retransmit: retransmit, BurstAt: -1,
	}
}

// Next returns the next n packets in transmit order.
func (g *RTPGen) Next(n int) []*rtp.Packet {
	out := make([]*rtp.Packet, 0, n)
	for len(out) < n {
		kept := g.queue[:0]
		for _, r := range g.queue {
			if r.at <= g.idx && len(out) < n {
				out = append(out, r.pkt)
			} else {
				kept = append(kept, r)
			}
		}
		g.queue = kept
		if len(out) >= n {
			break
		}
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: g.seq, Timestamp: g.ts, SSRC: 7},
			Payload: []byte{byte(g.seq), byte(g.idx)},
		}
		g.seq++
		g.ts += g.ticks
		i := g.idx
		g.idx++
		inBurst := g.BurstAt >= 0 && i >= g.BurstAt && i < g.BurstAt+g.BurstLen
		switch {
		case inBurst:
			// burst loss: never seen again
		case g.rng.Float64() < g.Loss:
			if g.rng.Float64() < g.Retransmit {
				g.queue = append(g.queue, retransmit{pkt, i + 2 + g.rng.Intn(3)})
			}
		default:
			out = append(out, pkt)
		}
	}
	return out
}

// Flush drains pending retransmissions.
func (g *RTPGen) Flush() []*rtp.Packet {
	out := make([]*rtp.Packet, 0, len(g.queue))
	for _, r := range g.queue {
		out = append(out, r.pkt)
	}
	g.queue = nil
	return out
}

// PCM generates deterministic pseudo-random PCM frames.
func PCM(seed int64, rate, channels, frames int) *audio.PCM {
	rng := rand.New(rand.NewSource(seed))
	s := make([]int16, frames*channels)
	for i := range s {
		s[i] = int16(rng.Intn(2001) - 1000)
	}
	return &audio.PCM{SampleRate: rate, Channels: channels, Samples: s}
}

// ColorSet returns per-track color metadata; conflict mixes BT.2020
// into the last track of an otherwise BT.709 set.
func ColorSet(tracks []string, conflict bool) []color.Meta {
	out := make([]color.Meta, 0, len(tracks))
	for i, tr := range tracks {
		m := color.Meta{Source: tr, Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
		if conflict && i == len(tracks)-1 {
			m.Primaries, m.Transfer, m.Matrix = "bt2020", "bt2020", "bt2020nc"
		}
		out = append(out, m)
	}
	return out
}
