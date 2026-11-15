// Package loadgen generates synthetic RTP packets on a deterministic,
// seeded schedule with configurable rate and injected jitter.
package loadgen

import (
	"encoding/binary"
	"math/rand"
	"time"

	rtp "github.com/pion/rtp/v2"
)

// Config controls packet generation.
type Config struct {
	Packets     int     // total packets to generate
	Rate        int     // packets per second
	JitterMs    float64 // injected one-way jitter amplitude (mean of |D|)
	Seed        int64   // PRNG seed; identical seeds give identical streams
	SSRC        uint32  // source SSRC
	PayloadSize int     // RTP payload bytes (>= 4; first 4 hold the index)
}

// Packet is one generated RTP packet with its modeled timing.
type Packet struct {
	Data   []byte
	SendAt time.Duration // modeled send instant
	RecvAt time.Duration // modeled arrival instant (SendAt + fixed latency)
}

// Generator produces packets on a deterministic schedule. Sends happen on
// the ideal uniform grid; the modeled arrival adds a delay d_i uniform on
// [0, 3*jitter], so the interarrival variation |D| = |d_i - d_{i-1}| has
// mean equal to JitterMs.
type Generator struct {
	cfg   Config
	delay []time.Duration
	rng   *rand.Rand
	i     int
}

// New builds a generator; the delay schedule is precomputed from Seed.
func New(cfg Config) *Generator {
	if cfg.PayloadSize < 4 {
		cfg.PayloadSize = 4
	}
	sched := rand.New(rand.NewSource(cfg.Seed))
	delay := make([]time.Duration, cfg.Packets)
	amp := 3 * cfg.JitterMs * float64(time.Millisecond)
	for i := range delay {
		delay[i] = time.Duration(sched.Float64() * amp)
	}
	return &Generator{
		cfg:   cfg,
		delay: delay,
		rng:   rand.New(rand.NewSource(cfg.Seed + 1)),
	}
}

// Schedule returns the modeled send/recv instants of packet idx: send is
// on the uniform grid, recv adds the per-packet jittered delay.
func (g *Generator) Schedule(idx int) (sendAt, recvAt time.Duration) {
	interval := time.Second / time.Duration(g.cfg.Rate)
	send := time.Duration(idx) * interval
	return send, send + time.Millisecond + g.delay[idx]
}

// Next returns the next packet, or ok=false when the stream is exhausted.
func (g *Generator) Next() (pkt Packet, ok bool) {
	if g.i >= g.cfg.Packets {
		return Packet{}, false
	}
	idx := g.i
	g.i++
	payload := make([]byte, g.cfg.PayloadSize)
	binary.BigEndian.PutUint32(payload, uint32(idx))
	g.rng.Read(payload[4:])
	tsStep := uint32(8000 / g.cfg.Rate)
	p := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			SequenceNumber: uint16(idx),
			Timestamp:      uint32(idx) * tsStep,
			SSRC:           g.cfg.SSRC,
		},
		Payload: payload,
	}
	data, err := p.Marshal()
	if err != nil {
		return Packet{}, false
	}
	send, recv := g.Schedule(idx)
	return Packet{Data: data, SendAt: send, RecvAt: recv}, true
}
