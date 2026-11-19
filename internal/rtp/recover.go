package rtp

import (
	"avpipe/internal/clock"
	"avpipe/internal/fault"
	"avpipe/internal/rng"
)

// Config controls the simulated loss and jitter of the receive path.
type Config struct {
	LossRate     float64 // [0,1) probability a packet is dropped
	JitterMs     float64 // uniform extra arrival delay, 0..JitterMs
	Seed         uint64
	Window       int // recovery window in packets; gaps larger than this give up
	LatePackets  int // number of duplicates to inject as late copies
	ConflictCopy int // duplicate copies that are mutated to test conflicts
}

// Delivery is one simulated packet arrival in receive order.
type Delivery struct {
	Pkt      Packet
	Arrival  clock.Tick // simulated arrival time on the media grid
	Dropped  bool
	Late     bool
	Conflict bool
}

// Reason classifies why a sequence was unrecoverable.
type Reason string

const (
	ReasonNone    Reason = ""
	ReasonWindow  Reason = "beyond_recovery_window"
	ReasonMissing Reason = "not_delivered"
)

// Gap is a contiguous run of missing sequence numbers.
type Gap struct {
	Start, End uint64 // extended sequence numbers, inclusive
	Count      int
	Reason     Reason
}

// Recovery summarizes the receive path.
//
// PacketRecoveryRate is the fraction of lost packets the recovery window can
// repair: a gap no wider than Window is assumed repaired by retransmission,
// while a wider gap is declared unrecoverable. It answers "how much of the
// loss is recoverable?" rather than restating the delivery ratio, so 8%
// random loss with a 16-packet window reports ~100% while a burst wider than
// the window visibly lowers the rate.
type Recovery struct {
	Sent          int     `json:"sent"`
	Delivered     int     `json:"delivered"`
	Duplicates    int     `json:"duplicates"`
	Conflicts     int     `json:"conflicts"`
	Lost          int     `json:"lost"`
	Recovered     int     `json:"recovered"`
	Recoverable   int     `json:"recoverable"`
	Unrecoverable int     `json:"unrecoverable"`
	Rate          float64 `json:"packet_recovery_rate"`
	Gaps          []Gap   `json:"gaps,omitempty"`
	Ordered       []Packet
}

// SimulateReceive runs packets through loss, jitter and duplication, then
// reconstructs the stream: sort by extended sequence, drop duplicates, flag
// conflicting duplicates and classify gaps by whether the window could have
// covered them.
func SimulateReceive(sent []Packet, cfg Config) (Recovery, error) {
	if cfg.LossRate < 0 || cfg.LossRate >= 1 {
		return Recovery{}, fault.New(fault.CodeInvalidArgument, "rtp.simulate",
			"loss rate %v outside [0,1)", cfg.LossRate)
	}
	if cfg.JitterMs < 0 {
		return Recovery{}, fault.New(fault.CodeInvalidArgument, "rtp.simulate",
			"jitter %v ms is negative", cfg.JitterMs)
	}
	window := cfg.Window
	if window <= 0 {
		window = 64
	}
	r := rng.New(cfg.Seed)

	deliveries := make([]Delivery, 0, len(sent)+cfg.LatePackets+cfg.ConflictCopy)
	for _, p := range sent {
		if r.Float() < cfg.LossRate {
			deliveries = append(deliveries, Delivery{Pkt: p, Dropped: true})
			continue
		}
		arrival := clock.FromMillis(r.Float() * cfg.JitterMs)
		deliveries = append(deliveries, Delivery{Pkt: p, Arrival: arrival})
	}
	// Late copies arrive as duplicates after the original stream.
	for i := 0; i < cfg.LatePackets && len(sent) > 0; i++ {
		p := sent[r.Intn(len(sent))]
		arrival := clock.FromMillis(float64(window) + r.Float()*cfg.JitterMs)
		deliveries = append(deliveries, Delivery{Pkt: p, Arrival: arrival, Late: true})
	}
	for i := 0; i < cfg.ConflictCopy && len(sent) > 0; i++ {
		p := sent[r.Intn(len(sent))]
		mutated := p
		mutated.Payload = append([]byte(nil), p.Payload...)
		if len(mutated.Payload) > 0 {
			mutated.Payload[0] ^= 0xFF
		}
		deliveries = append(deliveries, Delivery{Pkt: mutated, Conflict: true})
	}

	rec := Recovery{Sent: len(sent)}
	seen := make(map[uint64][]Packet)
	var exts []uint64
	minExt := ^uint64(0)
	maxExt := uint64(0)
	first := true
	for _, d := range deliveries {
		if d.Dropped {
			continue
		}
		rec.Delivered++
		var ref uint64
		if !first {
			ref = maxExt
		}
		ext := Extend(d.Pkt.Seq, ref)
		if _, ok := seen[ext]; !ok {
			exts = append(exts, ext)
		}
		seen[ext] = append(seen[ext], d.Pkt)
		if first || ext < minExt {
			minExt = ext
			first = false
		}
		if ext > maxExt {
			maxExt = ext
		}
	}
	if len(exts) == 0 {
		rec.Rate = 1
		return rec, nil
	}

	ordered := make([]extended, 0, len(exts))
	for _, ext := range exts {
		copies := seen[ext]
		base := copies[0]
		for _, c := range copies[1:] {
			if !equalPayload(base, c) {
				rec.Conflicts++
				break
			}
		}
		rec.Duplicates += len(copies) - 1
		ordered = append(ordered, extended{ext: ext, pkt: base})
	}
	sortPackets(ordered)
	rec.Ordered = make([]Packet, len(ordered))
	for i, e := range ordered {
		rec.Ordered[i] = e.pkt
	}

	total := int(maxExt-minExt) + 1
	rec.Lost = total - len(ordered)
	var gapStart int64 = -1
	flush := func(endExt uint64) {
		if gapStart < 0 {
			return
		}
		count := int(endExt) - int(gapStart) + 1
		g := Gap{Start: uint64(gapStart), End: endExt, Count: count}
		if count > window {
			g.Reason = ReasonWindow
			rec.Unrecoverable += count
		} else {
			g.Reason = ReasonMissing
		}
		rec.Gaps = append(rec.Gaps, g)
		gapStart = -1
	}
	prev := int64(minExt) - 1
	for _, e := range ordered {
		if int64(e.ext) > prev+1 {
			if gapStart < 0 {
				gapStart = prev + 1
			}
			// The run ends immediately before this packet; close it now so
			// a resumed stream does not swallow the gap.
			flush(uint64(e.ext - 1))
		}
		prev = int64(e.ext)
	}
	if gapStart >= 0 {
		flush(uint64(prev))
	}
	rec.Recoverable = rec.Lost - rec.Unrecoverable
	rec.Recovered = rec.Recoverable // repaired by retransmission within window
	if rec.Lost > 0 {
		rec.Rate = float64(rec.Recoverable) / float64(rec.Lost)
	} else {
		rec.Rate = 1
	}
	return rec, nil
}

func equalPayload(a, b Packet) bool {
	if a.Timestamp != b.Timestamp || a.Marker != b.Marker || len(a.Payload) != len(b.Payload) {
		return false
	}
	for i := range a.Payload {
		if a.Payload[i] != b.Payload[i] {
			return false
		}
	}
	return true
}

// MediaTicks converts an RTP timestamp at the given clock rate to 90 kHz
// ticks. rtpClock is the packetizer clock (e.g. 48000 for Opus, 90000 for
// video); conversion is exact integer arithmetic.
func MediaTicks(ts uint32, rtpClock uint32) clock.Tick {
	if rtpClock == 0 {
		return 0
	}
	out, _ := clock.Rescale(int64(ts), int64(rtpClock), clock.Timescale)
	return clock.Tick(out)
}
