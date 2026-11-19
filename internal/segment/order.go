// Package segment deduplicates, orders and gap-checks fetched segments.
// Dedup keys on (sequence number, content hash): identical copies are
// dropped, and same-sequence/different-content is recorded as a conflict
// instead of silently overwriting.
package segment

import (
	"crypto/sha256"
	"sort"

	"archive/internal/errs"
)

// Limits guard against corrupt or hostile inputs (spec: 20 GiB per asset,
// 100000 segments per asset).
const (
	MaxAssetBytes = int64(20) << 30
	MaxSegments   = 100000
)

// Segment is one fetched segment with its content hash.
type Segment struct {
	Index int // sequence number from the manifest / filename
	URI   string
	Data  []byte
	Hash  [sha256.Size]byte
}

// New builds a Segment and computes its content hash.
func New(index int, uri string, data []byte) Segment {
	return Segment{Index: index, URI: uri, Data: data, Hash: sha256.Sum256(data)}
}

// Conflict records two different contents claiming the same sequence
// number. The first-seen content is kept; the later one is rejected.
type Conflict struct {
	Index     int
	KeptURI   string
	KeptHash  [sha256.Size]byte
	DroppedURI string
	DroppedHash [sha256.Size]byte
}

// Result is the outcome of Order.
type Result struct {
	Ordered    []Segment // sorted by Index, deduplicated
	Gaps       []int     // missing sequence numbers inside [min, max]
	Conflicts  []Conflict
	Duplicates int // identical copies dropped
}

// Order deduplicates segs by (Index, Hash), sorts by Index and reports
// gaps against the [minIndex, maxIndex] span actually seen. expected, when
// >= 0, is the manifest-declared segment count: trailing missing segments
// (beyond the highest seen index) are then reported as gaps too.
func Order(asset string, segs []Segment, firstIndex, expected int) (*Result, error) {
	if len(segs) > MaxSegments {
		return nil, errs.Newf(errs.KindSegment, asset, "order", "segment count %d exceeds limit %d", len(segs), MaxSegments)
	}
	var total int64
	byIndex := map[int]int{} // index -> position in out
	r := &Result{}
	for _, s := range segs {
		total += int64(len(s.Data))
		if total > MaxAssetBytes {
			return nil, errs.Newf(errs.KindSegment, asset, s.URI, "asset size exceeds %d bytes", MaxAssetBytes)
		}
		pos, seen := byIndex[s.Index]
		if !seen {
			byIndex[s.Index] = len(r.Ordered)
			r.Ordered = append(r.Ordered, s)
			continue
		}
		kept := r.Ordered[pos]
		if kept.Hash == s.Hash {
			r.Duplicates++
			continue
		}
		// Same sequence number, different content: conflict. Keep the
		// first-seen copy; never silently overwrite.
		r.Conflicts = append(r.Conflicts, Conflict{
			Index: s.Index, KeptURI: kept.URI, KeptHash: kept.Hash,
			DroppedURI: s.URI, DroppedHash: s.Hash,
		})
	}
	sort.Slice(r.Ordered, func(i, j int) bool { return r.Ordered[i].Index < r.Ordered[j].Index })
	if len(r.Ordered) == 0 {
		return r, nil
	}
	lo, hi := r.Ordered[0].Index, r.Ordered[len(r.Ordered)-1].Index
	if firstIndex >= 0 && firstIndex < lo {
		lo = firstIndex
	}
	if expected >= 0 && firstIndex >= 0 && firstIndex+expected-1 > hi {
		hi = firstIndex + expected - 1
	}
	present := make(map[int]bool, len(r.Ordered))
	for _, s := range r.Ordered {
		present[s.Index] = true
	}
	for i := lo; i <= hi; i++ {
		if !present[i] {
			r.Gaps = append(r.Gaps, i)
		}
	}
	return r, nil
}
