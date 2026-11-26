package rtp

import (
	"mediamix/internal/clock"
)

// StreamConfig describes the locally synthesised audio track.
type StreamConfig struct {
	SSRC        uint32
	ClockRate   uint32 // RTP clock rate, e.g. 16000
	PayloadType uint8
	PacketSpan  uint32 // samples per RTP packet, e.g. 320 = 20 ms at 16 kHz
	PayloadSize int    // PCM bytes per packet
	StartSeq    uint16
	StartTS     uint32
}

// DefaultStreamConfig returns 16 kHz mono s16 PCM, 20 ms per packet.
func DefaultStreamConfig() StreamConfig {
	return StreamConfig{
		SSRC:        0x5151AAAA,
		ClockRate:   16000,
		PayloadType: 11,
		PacketSpan:  320,
		PayloadSize: 640,
		StartSeq:    100,
		StartTS:     5000,
	}
}

// FixtureConfig controls the impairment injection. All randomness derives
// from Seed so runs are fully reproducible.
type FixtureConfig struct {
	Seed               int64
	Seconds            int
	LossRate           float64
	ReorderRate        float64
	DuplicateRate      float64
	ForceSeqWrap       bool // place the stream end near a 16-bit seq wrap
	ForceTimestampWrap bool // place the start near a 32-bit timestamp wrap
	SenderReportEvery  int  // packets between SRs; 0 disables
}

// Event records an injected impairment for test assertions.
type Event struct {
	Kind string // "loss" | "reorder" | "duplicate" | "seq_wrap" | "ts_wrap"
	Seq  uint16
}

// Fixture is the generated capture.
type Fixture struct {
	Packets  []*Packet
	Reports  []*SenderReport
	Events   []Event
	Config   StreamConfig
	TotalSeq uint16 // number of packets generated before impairment
}

// lcg is a tiny deterministic PRNG (Numerical Recipes constants).
type lcg struct{ state uint64 }

func newLCG(seed int64) *lcg { return &lcg{state: uint64(seed)*6364136223846793005 + 1} }

func (g *lcg) float64() float64 {
	g.state = g.state*6364136223846793005 + 1442695040888963407
	return float64(g.state>>11) / float64(uint64(1)<<53)
}

// Generate builds the synthetic RTP stream and Sender Reports.
func Generate(sc StreamConfig, fc FixtureConfig) *Fixture {
	if fc.SenderReportEvery == 0 {
		fc.SenderReportEvery = 50
	}
	count := uint32(fc.Seconds) * sc.ClockRate / sc.PacketSpan
	startSeq := sc.StartSeq
	startTS := sc.StartTS
	if fc.ForceSeqWrap {
		startSeq = uint16(65536 - count/2)
	}
	if fc.ForceTimestampWrap {
		// Place the start half a stream before the 32-bit boundary so the
		// stream crosses it via natural unsigned wrap.
		startTS = 0 - count*sc.PacketSpan/2
	}
	rng := newLCG(fc.Seed)
	fx := &Fixture{Config: sc, TotalSeq: uint16(count)}

	// First build the ideal ordered packet list, then impair it.
	ideal := make([]*Packet, count)
	for i := uint32(0); i < count; i++ {
		seq := startSeq + uint16(i)
		ts := startTS + i*sc.PacketSpan
		payload := make([]byte, sc.PayloadSize)
		// Deterministic PCM-ish content: sequence-derived pattern so loss
		// positions remain visible in the output hash.
		for b := range payload {
			payload[b] = byte(uint32(seq)*7 + uint32(b)*13)
		}
		ideal[i] = &Packet{
			Version:     Version,
			PayloadType: sc.PayloadType,
			Seq:         seq,
			Timestamp:   ts,
			SSRC:        sc.SSRC,
			Marker:      i == count-1,
			Payload:     payload,
		}
		if fc.ForceTimestampWrap && i == count/2 {
			fx.Events = append(fx.Events, Event{Kind: "ts_wrap", Seq: seq})
		}
		if fc.ForceSeqWrap && seq < startSeq {
			fx.Events = append(fx.Events, Event{Kind: "seq_wrap", Seq: seq})
		}
	}

	dropped := make(map[uint16]bool)
	for i, p := range ideal {
		if i == 0 || i == len(ideal)-1 {
			continue // keep first and last so boundaries are well defined
		}
		if rng.float64() < fc.LossRate {
			dropped[p.Seq] = true
			fx.Events = append(fx.Events, Event{Kind: "loss", Seq: p.Seq})
		}
	}

	// Emit with reordering and duplicates.
	for i := 0; i < len(ideal); i++ {
		p := ideal[i]
		if dropped[p.Seq] {
			continue
		}
		if i > 1 && i < len(ideal)-2 && rng.float64() < fc.ReorderRate && len(fx.Packets) > 0 {
			// Swap with the previous emitted packet.
			prev := fx.Packets[len(fx.Packets)-1]
			fx.Packets[len(fx.Packets)-1] = p
			fx.Packets = append(fx.Packets, prev)
			fx.Events = append(fx.Events, Event{Kind: "reorder", Seq: p.Seq})
			continue
		}
		fx.Packets = append(fx.Packets, p)
		if i > 0 && i < len(ideal)-1 && rng.float64() < fc.DuplicateRate {
			fx.Packets = append(fx.Packets, p)
			fx.Events = append(fx.Events, Event{Kind: "duplicate", Seq: p.Seq})
		}
	}

	// Sender Reports: pair RTP ts with the equivalent reference tick.
	for i := fc.SenderReportEvery; i < len(ideal); i += fc.SenderReportEvery {
		p := ideal[i]
		tick := clock.FromSamples(int64(p.Timestamp-startTS), sc.ClockRate)
		fx.Reports = append(fx.Reports, &SenderReport{
			SSRC:         sc.SSRC,
			NTPTick:      tick,
			RTPTimestamp: p.Timestamp,
			PacketCount:  uint32(i + 1),
			OctetCount:   uint32(i+1) * uint32(sc.PayloadSize),
		})
	}
	return fx
}
