// Package index builds the random-access jump index over the repaired
// timeline. Entries point only at real decodable packet boundaries;
// positions inside loss gaps are never fabricated.
package index

import (
	"sort"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
)

// Entry is one random-access point: a decodable packet boundary.
type Entry struct {
	Time   clock.Time
	SeqExt uint64
}

// Index is an ordered list of random-access entries.
type Index struct {
	Entries []Entry
}

// Build places an entry at the first packet and then at the first
// decodable packet at or after each interval tick. Truncated packets are
// never used as access points.
func Build(packets []*event.Packet, interval clock.Time) *Index {
	idx := &Index{}
	if len(packets) == 0 || interval <= 0 {
		return idx
	}
	next := packets[0].MediaTime
	for _, p := range packets {
		if p.Flags&event.FlagTruncated != 0 {
			continue
		}
		if p.MediaTime >= next {
			idx.Entries = append(idx.Entries, Entry{Time: p.MediaTime, SeqExt: p.SeqExt})
			next = p.MediaTime + interval
		}
	}
	return idx
}

// Find returns the latest entry at or before t.
func (i *Index) Find(t clock.Time) (Entry, bool) {
	n := sort.Search(len(i.Entries), func(k int) bool { return i.Entries[k].Time > t })
	if n == 0 {
		return Entry{}, false
	}
	return i.Entries[n-1], true
}
