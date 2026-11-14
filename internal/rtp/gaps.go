package rtp

import "sort"

// bufferedReorderDepth counts packets held behind the contiguous frontier.
func (d *Depacketizer) bufferedReorderDepth() int {
	if _, ok := d.pending[d.next]; ok {
		return len(d.pending) - 1
	}
	return len(d.pending)
}

// recordGap merges adjacent intervals and keeps the counters in sync.
func (d *Depacketizer) recordGap(first, last uint16, count int) {
	if n := len(d.stats.Gaps); n > 0 {
		prev := d.stats.Gaps[n-1]
		if uint32(prev.Last)+1 == uint32(first) {
			d.stats.Gaps[n-1].Last = last
			d.stats.Gaps[n-1].Count += count
			d.refreshGapStats()
			return
		}
	}
	d.stats.Gaps = append(d.stats.Gaps, GapInterval{First: first, Last: last, Count: count})
	d.refreshGapStats()
}

func (d *Depacketizer) refreshGapStats() {
	d.stats.GapSegments = len(d.stats.Gaps)
	d.stats.GapPackets = 0
	for _, g := range d.stats.Gaps {
		d.stats.GapPackets += g.Count
	}
}

// finalizeGaps closes gaps still open at stream end, then drains the buffered
// contiguous tail into frames. Window counters are not adjusted here: by the
// time finalization runs, every unresolved loss is evidence, not backlog.
func (d *Depacketizer) finalizeGaps() (Stats, []GapInterval) {
	if len(d.pending) > 0 {
		seqs := make([]uint32, 0, len(d.pending))
		for seq := range d.pending {
			seqs = append(seqs, extend(d.basePos, seq))
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		lowest := seqs[0]
		nextExt := extend(d.basePos, d.next)
		if lowest > nextExt {
			d.recordGap(d.next, uint16(lowest-1), int(lowest-nextExt))
			d.next = uint16(lowest)
		}
		for {
			p, ok := d.pending[d.next]
			if !ok {
				break
			}
			delete(d.pending, d.next)
			delete(d.lateSeqs, d.next)
			d.deliver(p)
			d.next++
		}
	}
	return d.stats, append([]GapInterval(nil), d.stats.Gaps...)
}
