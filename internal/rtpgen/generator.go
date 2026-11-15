package rtpgen

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/pion/rtp"
)

type Config struct {
	PPS         int
	Duration    time.Duration
	Seed        int64
	Jitter      time.Duration
	SSRC        uint32
	PayloadType uint8
	ClockRate   uint32
}

type Generator struct {
	cfg Config
	rng *rand.Rand
}

type Result struct {
	Packets uint64        `json:"packets"`
	Elapsed time.Duration `json:"elapsed_ns"`
}

func New(cfg Config) (*Generator, error) {
	if cfg.PPS <= 0 {
		return nil, fmt.Errorf("pps must be positive")
	}
	if cfg.Duration <= 0 {
		return nil, fmt.Errorf("duration must be positive")
	}
	if cfg.SSRC == 0 {
		cfg.SSRC = 0x10309001
	}
	if cfg.PayloadType == 0 {
		cfg.PayloadType = 111
	}
	if cfg.ClockRate == 0 {
		cfg.ClockRate = 48000
	}
	return &Generator{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed))}, nil
}

func (g *Generator) Count() int {
	count := int(g.cfg.Duration * time.Duration(g.cfg.PPS) / time.Second)
	if count < 1 {
		return 1
	}
	return count
}

func (g *Generator) Run(ctx context.Context, emit func(*rtp.Packet) error) (Result, error) {
	start := time.Now()
	count := g.Count()
	interval := time.Second / time.Duration(g.cfg.PPS)
	var sent uint64
	for i := 0; i < count; i++ {
		due := start.Add(time.Duration(i) * interval)
		if g.cfg.Jitter > 0 {
			// A three-interval envelope models the short scheduling bursts seen
			// when audio frames and relay work share a CPU core.
			offset := time.Duration((g.rng.Float64()*2 - 1) * 3 * float64(g.cfg.Jitter))
			due = due.Add(offset)
		}
		if err := waitUntil(ctx, due); err != nil {
			return Result{Packets: sent, Elapsed: time.Since(start)}, nil
		}
		packet := g.packet(i)
		if err := emit(packet); err != nil {
			return Result{Packets: sent, Elapsed: time.Since(start)}, err
		}
		sent++
	}
	return Result{Packets: sent, Elapsed: time.Since(start)}, nil
}

func (g *Generator) packet(index int) *rtp.Packet {
	timestamp := uint64(index) * uint64(g.cfg.ClockRate) / uint64(g.cfg.PPS)
	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = byte(index + i)
	}
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    g.cfg.PayloadType,
			SequenceNumber: uint16(index),
			Timestamp:      uint32(timestamp),
			SSRC:           g.cfg.SSRC,
			Marker:         index%g.cfg.PPS == 0,
		},
		Payload: payload,
	}
}

func waitUntil(ctx context.Context, due time.Time) error {
	delay := time.Until(due)
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
