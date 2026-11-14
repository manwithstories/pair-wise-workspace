package relay

import (
	"math"
	"sort"
	"time"

	"github.com/pion/rtp"
)

const (
	// DefaultWindow is the default sequence-number reorder window.
	DefaultWindow = 64
	// DefaultReorderWait allows a short gap to be filled before skipping it.
	DefaultReorderWait = 15 * time.Millisecond
	// DefaultClockRate matches the video RTP fixture.
	DefaultClockRate uint32 = 90000
)

// JitterOptions configures sequence unwrapping and gap release.
type JitterOptions struct {
	Window      int
	ClockRate   uint32
	ReorderWait time.Duration
}

func (o JitterOptions) withDefaults() JitterOptions {
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.ClockRate == 0 {
		o.ClockRate = DefaultClockRate
	}
	if o.ReorderWait <= 0 {
		o.ReorderWait = DefaultReorderWait
	}
	return o
}

// JitterBuffer reorders by extended RTP sequence number, removes duplicate
// sequences, and drops packets that fall outside the configurable window.
type JitterBuffer struct {
	window      int64
	reorderWait time.Duration
	stats       *Stats
	estimator   *JitterEstimator

	packets map[int64]*rtp.Packet
	seen    map[int64]struct{}
	next    int64
	nextSet bool
	highest int64
	highSet bool
	gapAt   time.Time
}

// NewJitterBuffer creates the default 64-packet window at 90 kHz.
func NewJitterBuffer(stats *Stats) *JitterBuffer {
	return NewJitterBufferWithOptions(stats, JitterOptions{})
}

// NewJitterBufferWithOptions creates a buffer with explicit limits.
func NewJitterBufferWithOptions(stats *Stats, options JitterOptions) *JitterBuffer {
	if stats == nil {
		stats = NewStats()
	}
	options = options.withDefaults()
	return &JitterBuffer{
		window:      int64(options.Window),
		reorderWait: options.ReorderWait,
		stats:       stats,
		estimator:   NewJitterEstimator(options.ClockRate),
		packets:     make(map[int64]*rtp.Packet),
		seen:        make(map[int64]struct{}),
	}
}

// Push accepts one packet in arrival order and returns newly forwardable
// packets in sequence order.
func (b *JitterBuffer) Push(pkt *rtp.Packet, arrival time.Time) []*rtp.Packet {
	if pkt == nil {
		return nil
	}
	ext := b.extend(pkt.SequenceNumber)
	if _, ok := b.seen[ext]; ok {
		b.stats.IncDuplicate()
		return nil
	}
	b.seen[ext] = struct{}{}
	b.stats.IncReceived()
	if b.highSet && ext < b.highest {
		b.stats.IncReordered()
	}
	if !b.highSet || ext > b.highest {
		b.highest = ext
		b.highSet = true
	}
	if b.nextSet && ext < b.next {
		b.stats.IncLateDropped()
		return nil
	}
	if b.highSet && b.highest-ext > b.window {
		b.stats.IncLateDropped()
		return nil
	}

	b.packets[ext] = pkt
	b.stats.IncUnique()
	b.estimator.Record(pkt.Timestamp, arrival, func(ms float64) { b.stats.RecordJitter(ms) })
	if !b.nextSet {
		b.next = ext
		b.nextSet = true
	}
	out := b.emitContiguous()
	b.updateGap(arrival)
	return out
}

// Flush releases a gap after reorderWait, bounding latency when a sequence was
// genuinely lost. It also force-releases a gap wider than the window.
func (b *JitterBuffer) Flush(now time.Time) []*rtp.Packet {
	if len(b.packets) == 0 {
		return nil
	}
	out := b.emitContiguous()
	if len(b.packets) == 0 {
		b.gapAt = time.Time{}
		return out
	}
	if !b.gapAt.IsZero() && now.Sub(b.gapAt) < b.reorderWait && b.highest-b.next < b.window {
		return out
	}
	keys := b.sortedKeys()
	if len(keys) == 0 {
		return out
	}
	b.next = keys[0]
	b.nextSet = true
	out = append(out, b.emitContiguous()...)
	b.updateGap(now)
	return out
}

// FlushAll drains every buffered packet. It is used on orderly shutdown so
// forwarded equals unique whenever the send path succeeded.
func (b *JitterBuffer) FlushAll() []*rtp.Packet {
	keys := b.sortedKeys()
	out := make([]*rtp.Packet, 0, len(keys))
	for _, key := range keys {
		out = append(out, b.packets[key])
		delete(b.packets, key)
	}
	b.gapAt = time.Time{}
	return out
}

// Len reports the current buffered packet count.
func (b *JitterBuffer) Len() int { return len(b.packets) }

func (b *JitterBuffer) extend(seq uint16) int64 {
	if !b.highSet {
		return int64(seq)
	}
	return b.highest + int64(int16(seq-uint16(b.highest)))
}

func (b *JitterBuffer) emitContiguous() []*rtp.Packet {
	if !b.nextSet {
		return nil
	}
	var out []*rtp.Packet
	for {
		pkt, ok := b.packets[b.next]
		if !ok {
			break
		}
		delete(b.packets, b.next)
		out = append(out, pkt)
		b.next++
	}
	if len(out) > 0 && len(b.packets) > 0 {
		b.gapAt = time.Time{}
	}
	if len(b.packets) == 0 {
		b.gapAt = time.Time{}
	}
	return out
}

func (b *JitterBuffer) updateGap(now time.Time) {
	if len(b.packets) > 0 && b.gapAt.IsZero() {
		b.gapAt = now
	}
	if len(b.packets) == 0 {
		b.gapAt = time.Time{}
	}
}

func (b *JitterBuffer) sortedKeys() []int64 {
	keys := make([]int64, 0, len(b.packets))
	for key := range b.packets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// JitterEstimator implements the RFC 3550 interarrival jitter recurrence:
// J += (|D(i-1,i)| - J) / 16.
type JitterEstimator struct {
	clockRate   uint32
	initialized bool
	prevTS      uint32
	prevArrival time.Time
	jitter      float64
}

// NewJitterEstimator constructs an estimator for the RTP clock rate.
func NewJitterEstimator(clockRate uint32) *JitterEstimator {
	if clockRate == 0 {
		clockRate = DefaultClockRate
	}
	return &JitterEstimator{clockRate: clockRate}
}

// Record updates the RFC jitter estimate and returns the current value in ms.
func (e *JitterEstimator) Record(timestamp uint32, arrival time.Time, sample func(float64)) float64 {
	if !e.initialized {
		e.initialized = true
		e.prevTS = timestamp
		e.prevArrival = arrival
		return 0
	}
	tsDelta := float64(int64(int32(timestamp - e.prevTS)))
	arrivalDelta := float64(arrival.Sub(e.prevArrival).Nanoseconds()) * float64(e.clockRate) / float64(time.Second)
	d := math.Abs(arrivalDelta - tsDelta)
	e.jitter += (d - e.jitter) / 16.0
	e.prevTS = timestamp
	e.prevArrival = arrival
	ms := e.jitter * 1000 / float64(e.clockRate)
	if sample != nil {
		sample(ms)
	}
	return ms
}

// CurrentMS returns the latest smoothed estimate.
func (e *JitterEstimator) CurrentMS() float64 {
	return e.jitter * 1000 / float64(e.clockRate)
}
