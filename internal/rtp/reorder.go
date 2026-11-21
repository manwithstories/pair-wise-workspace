package rtp

import (
	"errors"
	"sort"

	"github.com/pion/rtp"
)

// ErrNilPacket is returned for arrivals carrying no packet.
var ErrNilPacket = errors.New("rtp: nil packet")

// Config tunes the reorder window and the bounded recovery policy.
type Config struct {
	WindowSize       int     // max packets held in the reorder window
	BaseWaitMs       float64 // reorder wait at the reference bandwidth
	RefBandwidthMbps float64 // reference bandwidth for BaseWaitMs
	MinWaitMs        float64 // lower clamp for the dynamic wait
	LatencyBudgetMs  float64 // hard upper clamp: the P95 latency budget
}

// waitLimit derives the reorder wait for the current link bandwidth.
// Lower bandwidth raises the wait, but it can never exceed the
// configured P95 latency budget.
func (c Config) waitLimit(bwMbps float64) float64 {
	w := c.BaseWaitMs
	if bwMbps > 0 && c.RefBandwidthMbps > 0 {
		w = c.BaseWaitMs * c.RefBandwidthMbps / bwMbps
	}
	if w < c.MinWaitMs {
		w = c.MinWaitMs
	}
	if w > c.LatencyBudgetMs {
		w = c.LatencyBudgetMs
	}
	return w
}

// Arrival is one received RTP packet with simulated link metadata.
type Arrival struct {
	Packet        *rtp.Packet
	ArrivalMs     float64 // arrival time on the unified ms base
	BandwidthMbps float64 // link bandwidth when the packet arrived
}

// Gap records one missing sequence number and how it was resolved.
type Gap struct {
	Seq        uint16
	DetectedMs float64
	Recovered  bool // filled within the bounded wait
	Late       bool // arrived after the wait expired: isolated
}

// Stats accumulates reorder and recovery counters.
type Stats struct {
	Received   int
	Duplicates int
	Recovered  int
	Late       int
	Lost       int
	Emitted    int
	Latencies  []float64
	Gaps       []Gap
}

// LossRecoveryRate is recovered / (recovered + lost). Late packets
// were isolated, not lost: they are excluded from both terms.
func (s Stats) LossRecoveryRate() float64 {
	den := s.Recovered + s.Lost
	if den == 0 {
		return 1
	}
	return float64(s.Recovered) / float64(den)
}

// LatencyP95 returns the 95th percentile emission latency in ms.
func (s Stats) LatencyP95() float64 {
	if len(s.Latencies) == 0 {
		return 0
	}
	sorted := make([]float64, len(s.Latencies))
	copy(sorted, s.Latencies)
	sort.Float64s(sorted)
	idx := (len(sorted)*95 + 99) / 100 // ceil(0.95*n)
	return sorted[idx-1]
}

// Reorderer puts RTP arrivals back in sequence order.
type Reorderer struct {
	cfg Config
}

// NewReorderer creates a Reorderer with the given configuration.
func NewReorderer(cfg Config) *Reorderer { return &Reorderer{cfg: cfg} }

// seqDiff compares sequence numbers wrap-aware (RFC 1982 style).
func seqDiff(a, b uint16) int { return int(int16(a - b)) }

// Process consumes arrivals in order and returns the de-duplicated,
// reordered packet stream plus recovery statistics.
//
//   - duplicates are counted but never re-emitted;
//   - an open gap filled within its bounded wait is recovered;
//   - a gap whose wait expires is declared lost at its exact deadline,
//     so head-of-line latency can never exceed the wait limit (which
//     is itself capped by the P95 latency budget);
//   - a packet arriving after its gap was closed is isolated (counted
//     late, not emitted); if it had been declared lost, the loss is
//     reclassified as late.
func (r *Reorderer) Process(arrivals []Arrival) ([]*rtp.Packet, Stats) {
	var st Stats
	type buffered struct {
		pkt *rtp.Packet
		at  float64
	}
	seen := make(map[uint16]bool, len(arrivals))
	lostSeqs := map[uint16]bool{}
	buf := map[uint16]buffered{}
	gapSince := map[uint16]float64{}
	var out []*rtp.Packet
	var expect uint16
	started := false
	var lastArrival, lastLimit float64

	emit := func(p buffered, now float64) {
		lat := now - p.at
		if lat < 0 {
			lat = 0
		}
		st.Latencies = append(st.Latencies, lat)
		out = append(out, p.pkt)
		st.Emitted++
	}
	drain := func(now float64) {
		for {
			p, ok := buf[expect]
			if !ok {
				return
			}
			emit(p, now)
			delete(buf, expect)
			expect++
		}
	}
	declareLost := func(seq uint16, detectedMs float64) {
		st.Lost++
		lostSeqs[seq] = true
		st.Gaps = append(st.Gaps, Gap{Seq: seq, DetectedMs: detectedMs})
		delete(gapSince, seq)
	}
	// expireHead declares the head gap lost once its deadline passes.
	// Blocked packets are emitted at the exact deadline so their
	// latency stays within the wait limit.
	expireHead := func(now, limit float64, windowFull bool) {
		for {
			if _, ok := buf[expect]; ok {
				return
			}
			t, isGap := gapSince[expect]
			if !isGap {
				return
			}
			deadline := t + limit
			if deadline > now && !windowFull {
				return
			}
			emitAt := deadline
			if windowFull && deadline > now {
				emitAt = now
			}
			declareLost(expect, t)
			expect++
			drain(emitAt)
		}
	}

	for _, a := range arrivals {
		if a.Packet == nil {
			continue
		}
		st.Received++
		lastArrival = a.ArrivalMs
		seq := a.Packet.SequenceNumber
		if seen[seq] {
			st.Duplicates++ // counted, never re-emitted
			continue
		}
		seen[seq] = true
		if !started {
			started = true
			expect = seq
		}
		limit := r.cfg.waitLimit(a.BandwidthMbps)
		lastLimit = limit
		// Expire the head gap at its deadline before this arrival.
		expireHead(a.ArrivalMs, limit, false)

		if seqDiff(seq, expect) < 0 {
			// Behind the head: fills an open gap, or is too late.
			if t, ok := gapSince[seq]; ok {
				delete(gapSince, seq)
				if a.ArrivalMs-t <= limit {
					st.Recovered++
					st.Gaps = append(st.Gaps, Gap{Seq: seq, DetectedMs: t, Recovered: true})
					buf[seq] = buffered{a.Packet, a.ArrivalMs}
					drain(a.ArrivalMs)
				} else {
					st.Late++
					st.Gaps = append(st.Gaps, Gap{Seq: seq, DetectedMs: t, Late: true})
				}
			} else {
				st.Late++ // isolated: gap already closed
				if lostSeqs[seq] {
					// It did arrive, only too late: reclassify.
					st.Lost--
					delete(lostSeqs, seq)
				}
			}
			continue
		}
		// Open a gap record for every skipped sequence number that
		// has not already arrived (buffered packets are not missing).
		for s := expect; seqDiff(s, seq) < 0; s++ {
			if _, ok := gapSince[s]; ok {
				continue
			}
			if _, arrived := buf[s]; arrived {
				continue
			}
			gapSince[s] = a.ArrivalMs
		}
		// A packet filling its own freshly opened or existing gap is a
		// recovery when it arrives inside the bounded wait.
		if t, isGap := gapSince[seq]; isGap {
			delete(gapSince, seq)
			if a.ArrivalMs-t <= limit {
				st.Recovered++
				st.Gaps = append(st.Gaps, Gap{Seq: seq, DetectedMs: t, Recovered: true})
			} else {
				st.Late++
				st.Gaps = append(st.Gaps, Gap{Seq: seq, DetectedMs: t, Late: true})
				continue // isolated: not emitted
			}
		}
		buf[seq] = buffered{a.Packet, a.ArrivalMs}
		drain(a.ArrivalMs)
		expireHead(a.ArrivalMs, limit, len(buf) > r.cfg.WindowSize)
	}

	// Flush: declare every remaining gap lost (head gaps emit their
	// blocked packets at the earlier of deadline and stream end),
	// then drain whatever is left in order.
	for {
		if p, ok := buf[expect]; ok {
			emit(p, lastArrival)
			delete(buf, expect)
			expect++
			continue
		}
		t, isGap := gapSince[expect]
		if !isGap {
			break
		}
		deadline := t + lastLimit
		declareLost(expect, t)
		expect++
		if deadline < lastArrival {
			drain(deadline)
		} else {
			drain(lastArrival)
		}
	}
	for s, t := range gapSince {
		declareLost(s, t)
	}
	return out, st
}
