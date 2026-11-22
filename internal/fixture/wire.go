package fixture

import (
	"fmt"
	"math/rand"

	"archivecheck/internal/clock"
	"archivecheck/internal/transport"
)

// WireConfig controls the simulated network path.
type WireConfig struct {
	// LossBurst is the length of a contiguous burst removed from the stream.
	LossBurst int
	// LossAt is the extended sequence number where the burst starts; -1 picks
	// a deterministic position from the seed.
	LossAt int
	// Reorder is how many adjacent packets get swapped at deterministic
	// positions.
	Reorder int
	// Duplicate is how many packets are delivered twice.
	Duplicate int
	// BandwidthMbps is the sampled throughput series in Mbps.
	BandwidthMbps []float64
}

// Wire is a generated RTP stream plus the reference timing.
type Wire struct {
	// Payload is the reassembled media payload the packets carry.
	Payload []byte
	// Packets is the arrival-order stream including duplicates and reordering.
	Packets []transport.Packet
	// Sent is the number of distinct packets the sender emitted.
	Sent int
	// Bandwidth is the per-window throughput series in bps.
	Bandwidth []transport.BandwidthSample
	// PacketBytes is the RTP payload size per packet.
	PacketBytes int
	// DurationSeconds is the media duration the payload spans.
	DurationSeconds float64
}

// packetSpan is 20 ms of audio at 48 kHz, 16-bit mono = 1920 bytes.
const packetSpan = 0.020

// BuildWire slices payload into 20 ms RTP packets and applies damage.
func BuildWire(seed int64, payload []byte, durationSeconds float64, cfg WireConfig) (*Wire, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("fixture: empty payload")
	}
	if durationSeconds <= 0 {
		return nil, fmt.Errorf("fixture: non-positive duration")
	}
	rng := rand.New(rand.NewSource(seed ^ 0x5A5A))
	// Packet size from duration: bytes/sec * span.
	size := int(float64(len(payload)) / durationSeconds * packetSpan)
	if size < 64 {
		size = 64
	}
	w := &Wire{Payload: payload, PacketBytes: size, DurationSeconds: durationSeconds}
	for off, seq := 0, 0; off < len(payload); off, seq = off+size, seq+1 {
		end := off + size
		if end > len(payload) {
			end = len(payload)
		}
		w.Packets = append(w.Packets, transport.Packet{
			Seq:     uint16(seq),
			Arrival: seq,
			Payload: append([]byte(nil), payload[off:end]...),
			Marker:  end == len(payload),
		})
	}
	w.Sent = len(w.Packets)

	// Burst loss.
	if cfg.LossBurst > 0 {
		start := cfg.LossAt
		if start < 0 {
			maxStart := len(w.Packets) - cfg.LossBurst - 1
			if maxStart <= 2 {
				start = 2
			} else {
				start = 2 + rng.Intn(maxStart-2)
			}
		}
		end := start + cfg.LossBurst
		kept := w.Packets[:0:0]
		for i, p := range w.Packets {
			if i >= start && i < end {
				continue
			}
			kept = append(kept, p)
		}
		w.Packets = kept
	}

	// Reorder: swap adjacent pairs at deterministic positions.
	for k := 0; k < cfg.Reorder && len(w.Packets) > 3; k++ {
		pos := 1 + (k*7)%(len(w.Packets)-2)
		w.Packets[pos], w.Packets[pos+1] = w.Packets[pos+1], w.Packets[pos]
	}

	// Duplicate: re-deliver packets slightly later in arrival order.
	for k := 0; k < cfg.Duplicate && len(w.Packets) > 1; k++ {
		pos := (k*11 + 3) % len(w.Packets)
		src := w.Packets[pos]
		insert := pos + 2
		if insert > len(w.Packets) {
			insert = len(w.Packets)
		}
		dup := src
		dup.Payload = append([]byte(nil), src.Payload...)
		w.Packets = append(w.Packets[:insert], append([]transport.Packet{dup}, w.Packets[insert:]...)...)
	}

	// Re-index arrival order after mutations.
	for i := range w.Packets {
		w.Packets[i].Arrival = i
	}

	// Bandwidth samples: one per logical second, clamped to a positive value.
	series := cfg.BandwidthMbps
	if len(series) == 0 {
		series = []float64{2}
	}
	points := int(durationSeconds/1.0) + 1
	if points < len(series) {
		points = len(series)
	}
	for i := 0; i < points; i++ {
		mbps := series[i%len(series)]
		if mbps <= 0 {
			mbps = 0.1
		}
		w.Bandwidth = append(w.Bandwidth, transport.BandwidthSample{
			At:  i * w.Sent / points,
			Bps: mbps * 1_000_000,
		})
	}
	return w, nil
}

// RTPWire returns wire bytes for the arrival-order packets.
func (w *Wire) RTPWire() ([][]byte, error) {
	out := make([][]byte, 0, len(w.Packets))
	for _, p := range w.Packets {
		b, err := p.ToRTP()
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// MediaDuration returns the exact duration as a clock.Rational.
func (w *Wire) MediaDuration() clock.Rational {
	return clock.MustParseDecimal(fmt.Sprintf("%g", w.DurationSeconds))
}
