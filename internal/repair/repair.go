// Package repair detects missing intervals on the canonical timeline and
// repairs regressive or missing timestamps. It never fabricates media:
// loss is reported as annotated gaps, not filled.
package repair

import (
	"fmt"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
)

// tsTolerance bounds the rounding error of per-packet clock conversion;
// deviations beyond it are treated as timestamp damage and repaired.
const tsTolerance = clock.Time(2)

// Gap is a missing interval bounded by recovered packets (or open at the
// stream tail).
type Gap struct {
	AfterSeq   uint64     // extended sequence number preceding the gap
	Count      uint64     // number of missing packets
	Start      clock.Time // estimated start of the missing interval
	End        clock.Time // estimated end; 0 when unbounded at the tail
	Source     string     // loss_burst | tail_truncation
	Confidence float64    // 1.0 bounded on both sides, 0.5 tail estimate
}

// Degradation records an explicit quality downgrade of the output.
type Degradation struct {
	Reason event.Reason
	Detail string
}

// Result is the repaired timeline plus its defect annotations.
type Result struct {
	Packets    []*event.Packet
	Gaps       []Gap
	Degraded   []Degradation
	RepairedTS int
}

// Fatal reports whether the result contains a degradation that makes safe
// recovery impossible (no decodable boundary, or wrap-around ambiguity).
func (r *Result) Fatal() bool {
	for _, d := range r.Degraded {
		if d.Reason == event.ReasonNoBoundary || d.Reason == event.ReasonWrapAmbiguous {
			return true
		}
	}
	return false
}

// Repair validates the canonical (sequence-ordered) timeline, repairs
// damaged timestamps, and derives missing intervals. Quarantined packets
// with valid headers beyond the last accepted packet are reported as tail
// truncation with reduced confidence.
func Repair(sorted []*event.Packet, quarantine []event.Quarantine) *Result {
	res := &Result{}
	if len(sorted) == 0 {
		res.Degraded = append(res.Degraded, Degradation{
			Reason: event.ReasonNoBoundary, Detail: "no decodable packets in stream",
		})
		return res
	}
	out := make([]*event.Packet, 0, len(sorted))
	var prev *event.Packet
	for _, p := range sorted {
		if prev != nil {
			res.gapBetween(prev, p)
			res.fixTimestamp(prev, p)
		}
		out = append(out, p)
		prev = p
	}
	res.Packets = out
	res.tailGap(quarantine)
	return res
}

// gapBetween records a loss interval when sequence numbers are not
// contiguous. A jump larger than half the 16-bit cycle is ambiguous with
// a wrap-around and is reported as an explicit failure instead of a gap.
func (r *Result) gapBetween(prev, p *event.Packet) {
	if p.SeqExt <= prev.SeqExt+1 {
		return
	}
	count := p.SeqExt - prev.SeqExt - 1
	if count > 0x8000 {
		r.Degraded = append(r.Degraded, Degradation{
			Reason: event.ReasonWrapAmbiguous,
			Detail: fmt.Sprintf("sequence gap of %d after %d exceeds half cycle", count, prev.SeqExt),
		})
		return
	}
	r.Gaps = append(r.Gaps, Gap{
		AfterSeq:   prev.SeqExt,
		Count:      count,
		Start:      prev.MediaTime + prev.Duration,
		End:        p.MediaTime,
		Source:     "loss_burst",
		Confidence: 1.0,
	})
}

// fixTimestamp repairs regressive or missing timestamps. Contiguous
// packets must start where the previous packet ended; after a loss gap a
// forward timestamp is trusted, a regressive one is re-estimated from the
// previous packet duration.
func (r *Result) fixTimestamp(prev, p *event.Packet) {
	expected := prev.MediaTime + prev.Duration
	if p.SeqExt == prev.SeqExt+1 {
		if absDiff(p.MediaTime, expected) > tsTolerance {
			p.MediaTime = expected
			p.Flags |= event.FlagRepairedTS
			r.RepairedTS++
		}
		return
	}
	if p.MediaTime <= expected {
		p.MediaTime = expected + clock.Time(p.SeqExt-prev.SeqExt-1)*prev.Duration
		p.Flags |= event.FlagRepairedTS
		r.RepairedTS++
	}
}

// tailGap reports quarantined packets beyond the last accepted sequence
// number as a tail truncation with unknown extent.
func (r *Result) tailGap(quarantine []event.Quarantine) {
	last := r.Packets[len(r.Packets)-1]
	var maxQ uint64
	found := false
	for _, q := range quarantine {
		if q.HasSeq && q.SeqExt > last.SeqExt && (!found || q.SeqExt > maxQ) {
			maxQ = q.SeqExt
			found = true
		}
	}
	if !found {
		return
	}
	count := maxQ - last.SeqExt
	r.Gaps = append(r.Gaps, Gap{
		AfterSeq:   last.SeqExt,
		Count:      count,
		Start:      last.MediaTime + last.Duration,
		End:        0,
		Source:     "tail_truncation",
		Confidence: 0.5,
	})
	r.Degraded = append(r.Degraded, Degradation{
		Reason: event.ReasonTruncated,
		Detail: fmt.Sprintf("%d tail packet(s) quarantined; stream extent unknown", count),
	})
}

func absDiff(a, b clock.Time) clock.Time {
	if a > b {
		return a - b
	}
	return b - a
}
