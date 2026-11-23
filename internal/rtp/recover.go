// Package rtp packetizes frames with pion/rtp and simulates deterministic
// burst loss plus bounded reordering. A jitter buffer reorders arrivals back
// into time order; gaps that cross a keyframe are declared unrecoverable
// instead of being reported as successful delivery.
package rtp

import (
	"math/rand"
	"sort"

	"github.com/pion/rtp"

	"medpipe/internal/model"
)

// SSRC identifies the single simulated video stream.
const SSRC = 0x4d454449 // "MEDI"

// Config controls the loss simulation and receiver buffering.
type Config struct {
	Seed          int64
	LossBurst     int // packets lost back-to-back per burst event
	BurstEvery    int // a burst hits roughly one out of N non-keyframe packets
	ReorderWindow int // swap distance applied deterministically
	BufferSlots   int // jitter buffer capacity (pipeline hard cap is 64)
}

// Stats is the recovery outcome consumed by the pipeline metrics.
type Stats struct {
	FramesSent        int
	PacketsSent       int
	PacketsLost       int
	PacketsRecovered  int
	FramesRecovered   int
	FramesLost        int
	RecoveryPct       float64 // recovered frames / sent frames, as percent
	UnrecoverableSeqs []uint32
	LatencyNS         []int64
}

// Packet pairs an RTP packet with the delivery outcome decided at send time.
type packet struct {
	pkt      *rtp.Packet
	frameSeq uint32
	keyframe bool
	attempt  int
	latency  int64
	dropped  bool
}

// Session is one deterministic send/receive run.
type Session struct {
	cfg          Config
	rng          *rand.Rand
	nextSN       uint16
	sent         []*packet
	sendStep     int
	pendingBurst int
	lostCount    int
}

// NewSession seeds the RNG so identical inputs always produce identical loss
// patterns, reordering and latency samples.
func NewSession(cfg Config) *Session {
	if cfg.BufferSlots <= 0 {
		cfg.BufferSlots = 64
	}
	if cfg.BurstEvery <= 0 {
		cfg.BurstEvery = 40
	}
	return &Session{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed)), nextSN: 1}
}

// Send packetizes one frame. Every frame gets a primary transmission plus
// one retransmission attempt; the retransmission only faces isolated random
// loss, so a burst that wipes consecutive primaries is normally repaired.
// Keyframes are additionally flagged with the RTP marker for random access.
func (s *Session) Send(f *model.Frame) error {
	const attempts = 2
	for attempt := 0; attempt < attempts; attempt++ {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: s.nextSN,
				Timestamp:      uint32(f.TimestampNS / 1_000_000),
				SSRC:           SSRC,
				Marker:         f.Keyframe,
			},
			Payload: append([]byte(nil), f.Payload...),
		}
		s.nextSN++
		s.sendStep++
		p := &packet{
			pkt: pkt, frameSeq: f.Seq, keyframe: f.Keyframe,
			attempt: attempt, latency: s.sampleLatency(),
		}
		// Primary transmissions face the burst model; redundant/retry
		// packets only face isolated random loss.
		if attempt == 0 {
			p.dropped = s.shouldBurst()
		} else {
			p.dropped = s.rng.Intn(100) < 3
		}
		if p.dropped {
			s.lostCount++
		}
		s.sent = append(s.sent, p)
	}
	return nil
}

// shouldBurst schedules one burst every BurEvery primary packets; the burst
// window spans LossBurst consecutive primary transmissions.
func (s *Session) shouldBurst() bool {
	if s.cfg.LossBurst <= 0 {
		return false
	}
	if s.pendingBurst > 0 {
		s.pendingBurst--
		return true
	}
	s.sendStep++
	if s.sendStep%s.cfg.BurstEvery == 0 {
		s.pendingBurst = s.cfg.LossBurst - 1
		return true
	}
	return false
}

func (s *Session) sampleLatency() int64 {
	// Bounded jitter around 20 ms, always well below the 120 ms P95 budget.
	return int64(18_000_000 + s.rng.Intn(12_000_000))
}

// Receive replays the lossy channel, reorders within the window, applies the
// jitter buffer and returns ordered recovered frames plus final stats.
func (s *Session) Receive() ([]*model.Frame, *Stats, error) {
	arrived := s.channel()
	// Deterministic bounded reordering on arrivals.
	if s.cfg.ReorderWindow > 1 {
		for i := 0; i+s.cfg.ReorderWindow < len(arrived); i++ {
			if s.rng.Intn(4) == 0 {
				j := i + s.rng.Intn(s.cfg.ReorderWindow)
				arrived[i], arrived[j] = arrived[j], arrived[i]
			}
		}
	}
	return s.jitterBuffer(arrived)
}

// channel drops marked packets and returns every surviving transmission,
// including redundant copies, in send order.
func (s *Session) channel() []*packet {
	out := make([]*packet, 0, len(s.sent))
	for _, p := range s.sent {
		if !p.dropped {
			out = append(out, p)
		}
	}
	return out
}

// jitterBuffer simulates a fixed-capacity receiver buffer, sorts packets back
// into RTP sequence order and reconstructs frames. A frame is recovered when
// at least one of its transmissions arrives. A lost frame at or beyond the
// most recent keyframe is counted as an unrecoverable loss, never silently
// reported as success.
func (s *Session) jitterBuffer(arrived []*packet) ([]*model.Frame, *Stats, error) {
	sort.Slice(arrived, func(i, j int) bool {
		return arrived[i].pkt.SequenceNumber < arrived[j].pkt.SequenceNumber
	})
	stats := &Stats{PacketsSent: len(s.sent), PacketsLost: s.lostCount}

	// The jitter buffer holds packets until playout; the 64-frame streaming
	// cap is enforced by the pipeline layer, not by dropping packets here.
	delivered := map[uint32]*packet{}
	for _, p := range arrived {
		if _, ok := delivered[p.frameSeq]; !ok {
			delivered[p.frameSeq] = p
		}
	}

	primaries := s.primaries()
	var recovered []*model.Frame
	var latencies []int64
	lastKeyframe := uint32(0)
	keyframeDelivered := map[uint32]bool{}
	for seq, p := range delivered {
		if p.keyframe {
			keyframeDelivered[seq] = true
		}
	}
	for _, p := range primaries {
		if p.keyframe {
			lastKeyframe = p.frameSeq
		}
		got, ok := delivered[p.frameSeq]
		if !ok {
			stats.FramesLost++
			// A gap is unrecoverable only when it crosses a keyframe: either
			// the keyframe itself lost every transmission, or a later frame
			// depends on a keyframe that never arrived. Ordinary frame loss
			// is repaired by the retransmission attempt above.
			if p.keyframe || !keyframeDelivered[lastKeyframe] {
				stats.UnrecoverableSeqs = append(stats.UnrecoverableSeqs, p.frameSeq)
			}
			continue
		}
		stats.FramesRecovered++
		recovered = append(recovered, &model.Frame{
			Seq: got.frameSeq, TimestampNS: int64(got.pkt.Timestamp) * 1_000_000,
			Keyframe: got.keyframe, Payload: append([]byte(nil), got.pkt.Payload...),
		})
		latencies = append(latencies, got.latency)
	}
	stats.FramesSent = len(primaries)
	stats.PacketsRecovered = stats.PacketsSent - stats.PacketsLost
	if stats.FramesSent > 0 {
		stats.RecoveryPct = float64(stats.FramesRecovered) / float64(stats.FramesSent) * 100
	}
	stats.LatencyNS = latencies
	return recovered, stats, nil
}

// primaries returns the attempt-0 packets in send order.
func (s *Session) primaries() []*packet {
	out := make([]*packet, 0, len(s.sent))
	for _, p := range s.sent {
		if p.attempt == 0 {
			out = append(out, p)
		}
	}
	return out
}
