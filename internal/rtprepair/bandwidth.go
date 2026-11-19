package rtprepair

import (
	"fmt"
	"sort"
	"time"
)

// BandwidthProfile models a fluctuating uplink: the link speed changes every
// Interval, cycling through the listed rates in kilobits per second. Traffic
// is shaped by a token bucket with a bounded burst, so a slow interval delays
// packets instead of dropping them, and a fast interval drains the backlog.
type BandwidthProfile struct {
	Kbps     []int         `json:"kbps"`
	Interval time.Duration `json:"interval"`
	// BurstBytes is the largest instantaneous allowance; 0 defaults to one
	// second at the slowest declared rate.
	BurstBytes int `json:"burst_bytes"`
	// MaxDelay is the cap on any single packet's queuing delay. A packet that
	// would exceed it is marked Dropped and counted, which is how a sustained
	// bandwidth collapse turns into observable loss.
	MaxDelay time.Duration `json:"max_delay"`
}

// DefaultBandwidthProfile mirrors the demo command line.
func DefaultBandwidthProfile(kbps []int) BandwidthProfile {
	return BandwidthProfile{Kbps: kbps, Interval: 250 * time.Millisecond, MaxDelay: 2 * time.Second}
}

func (p BandwidthProfile) normalize() BandwidthProfile {
	if len(p.Kbps) == 0 {
		p.Kbps = []int{800}
	}
	for i, v := range p.Kbps {
		if v < 1 {
			p.Kbps[i] = 1
		}
	}
	if p.Interval <= 0 {
		p.Interval = 250 * time.Millisecond
	}
	if p.BurstBytes <= 0 {
		slowest := p.Kbps[0]
		for _, v := range p.Kbps {
			if v < slowest {
				slowest = v
			}
		}
		p.BurstBytes = slowest * 1000 / 8
		if p.BurstBytes < 1500 {
			p.BurstBytes = 1500
		}
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 2 * time.Second
	}
	return p
}

// RateAt returns the link rate in effect at elapsed time t.
func (p BandwidthProfile) RateAt(t time.Duration) int {
	if len(p.Kbps) == 0 {
		return 1
	}
	slot := int(t / p.Interval)
	if slot < 0 {
		slot = 0
	}
	return p.Kbps[slot%len(p.Kbps)]
}

// BandwidthResult is the shaping outcome for one packet stream.
type BandwidthResult struct {
	Arrivals     []time.Duration   `json:"-"`
	Dropped      []bool            `json:"-"`
	DroppedCount int               `json:"dropped"`
	MaxDelay     time.Duration     `json:"max_delay"`
	TotalBytes   int               `json:"total_bytes"`
	Elapsed      time.Duration     `json:"elapsed"`
	MinKbps      int               `json:"min_kbps"`
	MaxKbps      int               `json:"max_kbps"`
	Samples      []BandwidthSample `json:"samples"`
}

// BandwidthSample is one measurement window of observed throughput.
type BandwidthSample struct {
	StartMS   int64 `json:"start_ms"`
	EndMS     int64 `json:"end_ms"`
	Kbps      int   `json:"observed_kbps"`
	LinkKbps  int   `json:"link_kbps"`
	Delivered int   `json:"packets"`
	Dropped   int   `json:"dropped"`
}

// Shape assigns an arrival time to each packet under the profile. The input
// order is the send order; the returned slice is indexed the same way.
func Shape(packets [][]byte, profile BandwidthProfile) BandwidthResult {
	p := profile.normalize()
	res := BandwidthResult{MinKbps: p.Kbps[0], MaxKbps: p.Kbps[0]}
	for _, v := range p.Kbps {
		if v < res.MinKbps {
			res.MinKbps = v
		}
		if v > res.MaxKbps {
			res.MaxKbps = v
		}
	}
	res.Arrivals = make([]time.Duration, len(packets))
	res.Dropped = make([]bool, len(packets))
	tokens := float64(p.BurstBytes)
	var now time.Duration
	var last time.Duration
	type window struct {
		link, delivered, dropped int
		start                    time.Duration
	}
	var current window
	var samples []BandwidthSample
	for i, pkt := range packets {
		rate := p.RateAt(now)
		// Refill tokens for the time actually consumed since the last packet.
		dt := now - last
		if dt < 0 {
			dt = 0
		}
		tokens += float64(rate) * 1000 / 8 * dt.Seconds()
		last = now
		if cap := float64(p.BurstBytes); tokens > cap {
			tokens = cap
		}
		size := float64(len(pkt))
		res.TotalBytes += len(pkt)
		// A packet is admitted only once the bucket holds its full size. The
		// bucket is refilled at the current link rate, so a burst that exceeds
		// the profile is spread over multiple intervals.
		if tokens < size {
			need := size - tokens
			wait := time.Duration(need / (float64(rate) * 1000 / 8) * float64(time.Second))
			if wait > p.MaxDelay {
				res.Dropped[i] = true
				res.DroppedCount++
				res.Arrivals[i] = now
				if current.link == 0 {
					current.link, current.start = rate, now
				}
				current.dropped++
				now += time.Microsecond
				last = now
				continue
			}
			now += wait
			tokens += float64(rate) * 1000 / 8 * wait.Seconds()
			last = now
			rate = p.RateAt(now)
		}
		tokens -= size
		res.Arrivals[i] = now
		if current.link == 0 {
			current.link, current.start = rate, now
		}
		current.delivered++
		if current.link != 0 && now-current.start >= p.Interval {
			samples = append(samples, BandwidthSample{
				StartMS: current.start.Milliseconds(), EndMS: now.Milliseconds(),
				Kbps: observedKbps(current.delivered, now-current.start), LinkKbps: current.link,
				Delivered: current.delivered, Dropped: current.dropped,
			})
			current = window{}
		}
		// Advance wall time slightly for the next packet's scheduling.
		now += time.Microsecond
	}
	if current.delivered+current.dropped > 0 {
		samples = append(samples, BandwidthSample{
			StartMS: current.start.Milliseconds(), EndMS: now.Milliseconds(),
			Kbps: observedKbps(current.delivered, now-current.start), LinkKbps: current.link,
			Delivered: current.delivered, Dropped: current.dropped,
		})
	}
	res.Elapsed = now
	res.Samples = samples
	for i := range res.Arrivals {
		if res.Dropped[i] {
			continue
		}
		if d := res.Arrivals[i] - time.Duration(i)*time.Microsecond; d > res.MaxDelay {
			res.MaxDelay = d
		}
	}
	sort.SliceStable(samples, func(a, b int) bool { return samples[a].StartMS < samples[b].StartMS })
	return res
}

func observedKbps(packets int, span time.Duration) int {
	if span <= 0 {
		return 0
	}
	// Assume a nominal 1200-byte packet when the real size is unavailable.
	bytes := float64(packets) * 1200
	return int(bytes * 8 / 1000 / span.Seconds())
}

// Summary renders the shaping outcome for terminal output.
func (r BandwidthResult) Summary() string {
	dropPct := 0.0
	if n := len(r.Dropped); n > 0 {
		dropPct = 100 * float64(r.DroppedCount) / float64(n)
	}
	return fmt.Sprintf("link=%d..%dkbps windows=%d shaped_drop=%d (%.1f%%) elapsed=%.2fs",
		r.MinKbps, r.MaxKbps, len(r.Samples), r.DroppedCount, dropPct, r.Elapsed.Seconds())
}
