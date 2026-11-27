// Package nettrace reorders, deduplicates and repairs block streams that
// arrived over a lossy link, using the redundancy copies each block carries.
package nettrace

import (
	"sort"

	"avcheck/internal/wav"
)

// Item is one slot in the recovered sequence: either a block or a gap.
type Item struct {
	Seq       uint64
	Block     *wav.Block // nil when Gap is true
	Gap       bool
	Recovered bool // restored from a redundancy copy
}

// Stats summarizes recovery for reporting.
type Stats struct {
	Received   int // distinct blocks that arrived
	Duplicates int // repeated arrivals
	Lost       int // sequence numbers missing from the arrival stream
	Recovered  int // of Lost, restored via redundancy
	Gaps       int // of Lost, left as gap markers (never silently shifted)
}

// Recover orders the arrival stream of one track over the expected sequence
// range [first, last]. Missing blocks are restored from neighbours'
// redundancy copies (each block carries the two previous blocks); anything
// still missing becomes an explicit gap marker.
func Recover(arrived []*wav.Block, first, last uint64) ([]Item, Stats) {
	var st Stats
	bySeq := map[uint64]*wav.Block{}
	for _, b := range arrived {
		if b.Seq < first || b.Seq > last {
			continue
		}
		if _, dup := bySeq[b.Seq]; dup {
			st.Duplicates++
			continue
		}
		bySeq[b.Seq] = b
	}
	st.Received = len(bySeq)

	items := make([]Item, 0, last-first+1)
	for seq := first; seq <= last; seq++ {
		if b, ok := bySeq[seq]; ok {
			items = append(items, Item{Seq: seq, Block: b})
			continue
		}
		st.Lost++
		if r, ok := findRedundant(bySeq, seq); ok {
			st.Recovered++
			items = append(items, Item{Seq: seq, Block: r, Recovered: true})
			continue
		}
		st.Gaps++
		items = append(items, Item{Seq: seq, Gap: true})
	}
	return items, st
}

// findRedundant looks for a redundancy copy of seq in the blocks that did
// arrive: seq+1 carries it as Red[0], seq+2 as Red[1].
func findRedundant(bySeq map[uint64]*wav.Block, seq uint64) (*wav.Block, bool) {
	if n, ok := bySeq[seq+1]; ok && n.Red[0].Samples != nil && n.Red[0].Seq == seq {
		return &wav.Block{Seq: seq, Track: n.Track, Rate: n.Rate,
			Layout: n.Layout, Samples: n.Red[0].Samples}, true
	}
	if n, ok := bySeq[seq+2]; ok && n.Red[1].Samples != nil && n.Red[1].Seq == seq {
		return &wav.Block{Seq: seq, Track: n.Track, Rate: n.Rate,
			Layout: n.Layout, Samples: n.Red[1].Samples}, true
	}
	return nil, false
}

// SortArrival is a helper for tests and fixtures: stable order by arrival.
func SortArrival(blocks []*wav.Block) []*wav.Block {
	out := make([]*wav.Block, len(blocks))
	copy(out, blocks)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}
