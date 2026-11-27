package pipeline

import (
	"time"
)

// LatencyStats summarises per-event consumer latency.
type LatencyStats struct {
	Count int
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
	Max   time.Duration
}

// latencyWindow keeps a bounded reservoir of samples so a long run cannot
// grow memory without limit while still giving a stable P95.
type latencyWindow struct {
	values []time.Duration
	next   int
	total  int
}

func newLatencyWindow(capacity int) *latencyWindow {
	if capacity <= 0 {
		capacity = 4096
	}
	return &latencyWindow{values: make([]time.Duration, 0, capacity)}
}

func (w *latencyWindow) add(d time.Duration) {
	if len(w.values) < cap(w.values) {
		w.values = append(w.values, d)
	} else {
		w.values[w.next] = d
		w.next = (w.next + 1) % len(w.values)
	}
	w.total++
}

func (w *latencyWindow) stats() LatencyStats {
	if len(w.values) == 0 {
		return LatencyStats{}
	}
	sorted := append([]time.Duration(nil), w.values...)
	sortDurations(sorted)
	at := func(q float64) time.Duration {
		if len(sorted) == 1 {
			return sorted[0]
		}
		idx := int(q * float64(len(sorted)-1))
		return sorted[idx]
	}
	return LatencyStats{
		Count: w.total,
		P50:   at(0.50),
		P95:   at(0.95),
		P99:   at(0.99),
		Max:   sorted[len(sorted)-1],
	}
}

func sortDurations(d []time.Duration) {
	// Insertion sort is plenty for the bounded window and avoids pulling in
	// a comparator closure on the hot path.
	for i := 1; i < len(d); i++ {
		v := d[i]
		j := i - 1
		for j >= 0 && d[j] > v {
			d[j+1] = d[j]
			j--
		}
		d[j+1] = v
	}
}
