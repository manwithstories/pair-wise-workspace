package stats

import (
	"math"
	"sort"
	"sync"
	"time"
)

type JitterSummary struct {
	Samples   int     `json:"samples"`
	MeanMs    float64 `json:"mean_ms"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
	MaxMs     float64 `json:"max_ms"`
	RFC3550Ms float64 `json:"rfc3550_ms"`
}

type Summary struct {
	Received   uint64        `json:"received"`
	Lost       uint64        `json:"lost"`
	OutOfOrder uint64        `json:"out_of_order"`
	Duplicated uint64        `json:"duplicated"`
	Jitter     JitterSummary `json:"jitter"`
}

type Tracker struct {
	mu          sync.Mutex
	clockRate   uint32
	started     bool
	highest     int64
	first       int64
	seen        map[int64]struct{}
	prevTransit float64
	jitter      float64
	samples     []float64
	received    uint64
	lost        uint64
	outOfOrder  uint64
	duplicated  uint64
}

func NewTracker(clockRate uint32) *Tracker {
	if clockRate == 0 {
		clockRate = 48000
	}
	return &Tracker{clockRate: clockRate, seen: make(map[int64]struct{})}
}

func (t *Tracker) Update(sequenceNumber uint16, timestamp uint32, arrival time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	extended := t.extend(sequenceNumber)
	if _, exists := t.seen[extended]; exists {
		t.duplicated++
		return
	}
	t.seen[extended] = struct{}{}
	t.received++
	if t.started && extended < t.highest {
		t.outOfOrder++
	}
	if !t.started || extended > t.highest {
		t.highest = extended
	}
	if t.started && extended < t.first {
		t.first = extended
	}
	if t.started {
		transit := float64(arrival.UnixNano())/1e9*float64(t.clockRate) - float64(timestamp)
		delta := math.Abs(transit - t.prevTransit)
		t.jitter += (delta - t.jitter) / 16
		// Percentiles use the per-packet RFC3550 interarrival deviation D.
		// The smoothed estimator is retained separately as RFC3550Ms.
		t.samples = append(t.samples, delta/float64(t.clockRate)*1000)
	} else {
		t.first = extended
		t.highest = extended
		t.started = true
	}
	t.prevTransit = float64(arrival.UnixNano())/1e9*float64(t.clockRate) - float64(timestamp)
	if expected := t.highest - t.first + 1; expected > int64(t.received) {
		t.lost = uint64(expected - int64(t.received))
	} else {
		t.lost = 0
	}
}

func (t *Tracker) extend(sequenceNumber uint16) int64 {
	if !t.started {
		return int64(sequenceNumber)
	}
	delta := int16(sequenceNumber - uint16(t.highest))
	return t.highest + int64(delta)
}

func (t *Tracker) Summary() Summary {
	t.mu.Lock()
	defer t.mu.Unlock()
	samples := append([]float64(nil), t.samples...)
	sort.Float64s(samples)
	summary := Summary{
		Received: t.received, Lost: t.lost, OutOfOrder: t.outOfOrder, Duplicated: t.duplicated,
		Jitter: JitterSummary{Samples: len(samples)},
	}
	if len(samples) == 0 {
		return summary
	}
	var total float64
	for _, sample := range samples {
		total += sample
	}
	summary.Jitter.MeanMs = total / float64(len(samples))
	summary.Jitter.P50Ms = percentile(samples, 0.50)
	summary.Jitter.P95Ms = percentile(samples, 0.95)
	summary.Jitter.MaxMs = samples[len(samples)-1]
	summary.Jitter.RFC3550Ms = t.jitter / float64(t.clockRate) * 1000
	return summary
}

func percentile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(fraction*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
