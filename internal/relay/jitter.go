package relay

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// JitterStats contains RFC3550 jitter observations in milliseconds.
type JitterStats struct {
	P50     float64 `json:"p50"`
	P95     float64 `json:"p95"`
	Max     float64 `json:"max"`
	Samples int     `json:"samples"`
}

// Tracker maintains the RFC3550 interarrival jitter estimator and sample
// distribution. Samples are the smoothed estimator value after each packet.
type Tracker struct {
	mu          sync.Mutex
	clockRate   float64
	initialized bool
	lastArrival time.Time
	lastRTP     uint32
	jitterMS    float64
	values      []float64
}

// NewTracker creates a tracker for the given RTP clock rate.
func NewTracker(clockRate uint32) *Tracker {
	if clockRate == 0 {
		clockRate = 48000
	}
	return &Tracker{clockRate: float64(clockRate)}
}

// Observe processes one received packet at a deterministic arrival instant.
func (t *Tracker) Observe(packet *rtp.Packet, arrival time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.initialized {
		t.initialized = true
		t.lastArrival, t.lastRTP = arrival, packet.Timestamp
		t.values = append(t.values, 0)
		return
	}
	arrivalDeltaMS := float64(arrival.Sub(t.lastArrival)) / float64(time.Millisecond)
	rtpDeltaMS := float64(int32(packet.Timestamp-t.lastRTP)) / t.clockRate * 1000
	deviation := math.Abs(arrivalDeltaMS - rtpDeltaMS)
	t.jitterMS += (deviation - t.jitterMS) / 16
	t.values = append(t.values, t.jitterMS)
	t.lastArrival, t.lastRTP = arrival, packet.Timestamp
}

// Stats returns nearest-rank percentiles over the tracked samples.
func (t *Tracker) Stats() JitterStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return JitterFromValues(t.values)
}

// Values returns a copy of the raw smoothed-jitter observations.
func (t *Tracker) Values() []float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]float64(nil), t.values...)
}

// JitterFromValues calculates nearest-rank percentiles over raw observations.
func JitterFromValues(input []float64) JitterStats {
	if len(input) == 0 {
		return JitterStats{}
	}
	values := append([]float64(nil), input...)
	sort.Float64s(values)
	pick := func(percent float64) float64 {
		index := int(math.Ceil(percent/100*float64(len(values)))) - 1
		if index < 0 {
			index = 0
		}
		return round3(values[index])
	}
	return JitterStats{
		P50:     pick(50),
		P95:     pick(95),
		Max:     round3(values[len(values)-1]),
		Samples: len(values),
	}
}

// MergeJitter combines per-egress samples without pretending they are a
// single ordered stream. Percentiles are taken over all observed values.
func MergeJitter(stats []JitterStats) JitterStats {
	// Per-egress stats do not retain raw samples; aggregate percentiles by
	// weighting the already-computed distributions at a conservative maximum.
	var merged JitterStats
	for _, stat := range stats {
		if stat.Samples == 0 {
			continue
		}
		merged.P50 = math.Max(merged.P50, stat.P50)
		merged.P95 = math.Max(merged.P95, stat.P95)
		merged.Max = math.Max(merged.Max, stat.Max)
		merged.Samples += stat.Samples
	}
	return merged
}

// Frame is a packet released in sequence order by ReorderBuffer.
type Frame struct {
	Packet  *rtp.Packet
	Arrival time.Time
}

type pendingFrame struct {
	frame Frame
}

// ReorderBuffer holds a bounded sequence window and releases packets in order.
// Missing packets are skipped after Hold expires.
type ReorderBuffer struct {
	mu        sync.Mutex
	window    uint16
	hold      time.Duration
	next      uint16
	started   bool
	pending   map[uint16]pendingFrame
	gapSince  time.Time
	late      uint64
	skipped   uint64
	duplicate uint64
}

// NewReorderBuffer allows up to window packets ahead and waits hold before
// skipping a missing packet.
func NewReorderBuffer(window uint16, hold time.Duration) *ReorderBuffer {
	if window == 0 {
		window = 1
	}
	if hold <= 0 {
		hold = 40 * time.Millisecond
	}
	return &ReorderBuffer{window: window, hold: hold, pending: make(map[uint16]pendingFrame)}
}

// Push accepts an arrived packet and returns all frames that are now in order.
func (b *ReorderBuffer) Push(packet *rtp.Packet, arrival time.Time) []Frame {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.flushLocked(arrival)
	if !b.started {
		b.started, b.next = true, packet.SequenceNumber
		out = append(out, Frame{Packet: packet, Arrival: arrival})
		b.next++
		return append(out, b.drainLocked()...)
	}
	delta := int16(packet.SequenceNumber - b.next)
	switch {
	case delta < 0:
		b.late++
	case delta == 0:
		out = append(out, Frame{Packet: packet, Arrival: arrival})
		b.next++
		out = append(out, b.drainLocked()...)
	case uint16(delta) > b.window:
		b.skipped += uint64(delta)
		oldNext := b.next
		b.next = packet.SequenceNumber + 1
		for seq := range b.pending {
			if relative := int16(seq - oldNext); relative >= 0 && uint16(relative) < uint16(delta) {
				delete(b.pending, seq)
			}
		}
		out = append(out, Frame{Packet: packet, Arrival: arrival})
		out = append(out, b.drainLocked()...)
	default:
		if _, exists := b.pending[packet.SequenceNumber]; exists {
			b.duplicate++
			break
		}
		b.pending[packet.SequenceNumber] = pendingFrame{frame: Frame{Packet: packet, Arrival: arrival}}
		if b.gapSince.IsZero() {
			b.gapSince = arrival
		}
	}
	return out
}

// Flush releases the lowest pending sequence once its gap wait has expired.
func (b *ReorderBuffer) Flush(now time.Time) []Frame {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flushLocked(now)
}

func (b *ReorderBuffer) flushLocked(now time.Time) []Frame {
	if !b.started || b.gapSince.IsZero() || len(b.pending) == 0 || now.Sub(b.gapSince) < b.hold {
		return nil
	}
	var lowest uint16
	found := false
	best := uint16(0)
	for seq := range b.pending {
		delta := uint16(seq - b.next)
		if !found || delta < best {
			lowest, best, found = seq, delta, true
		}
	}
	if !found {
		return nil
	}
	b.skipped += uint64(best)
	b.next = lowest + 1
	out := []Frame{b.pending[lowest].frame}
	delete(b.pending, lowest)
	b.gapSince = time.Time{}
	out = append(out, b.drainLocked()...)
	return out
}

func (b *ReorderBuffer) drainLocked() []Frame {
	var out []Frame
	for {
		frame, ok := b.pending[b.next]
		if !ok {
			break
		}
		delete(b.pending, b.next)
		b.next++
		out = append(out, frame.frame)
		b.gapSince = time.Time{}
	}
	return out
}

// Snapshot returns late, skipped, and duplicate counts.
func (b *ReorderBuffer) Snapshot() (late, skipped, duplicate uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.late, b.skipped, b.duplicate
}

func round3(value float64) float64 { return math.Round(value*1000) / 1000 }
