// Package repair orders fragments deterministically and rebuilds damaged
// decode timelines without discarding the original values.
package repair

import (
	"fmt"
	"sort"

	"archivecheck/internal/fault"
	"archivecheck/internal/parse"
)

// SequenceIssue records a deterministic ordering decision.
type SequenceIssue struct {
	Segment   string
	Sequence  uint32
	Index     int
	Kind      string // "missing", "duplicate", "out_of_order"
	Detail    string
	Original  int
	Duplicate bool
}

// Ordered is the deterministic fragment order produced from an input list.
type Ordered struct {
	Fragments []*parse.Fragment
	Issues    []SequenceIssue
	Missing   int
}

type numbered struct {
	frag  *parse.Fragment
	index int
	seq   uint32
	hasSq bool
}

// Order builds a deterministic sequence from archive input order. Fragments
// with mfhd gaps are placed by their declared sequence number; missing numbers
// are reported exactly once; byte-identical duplicates are dropped; and
// conflicting duplicates or declared numbers that contradict manifest order
// are rejected with an offset-bearing fault.
func Order(frags []*parse.Fragment) (*Ordered, error) {
	out := &Ordered{}
	seenSeq := map[uint32]int{}
	items := make([]numbered, 0, len(frags))
	for i, f := range frags {
		if f == nil {
			return nil, fault.New(fault.KindSequence, "", "moof", 0, "nil fragment in input order")
		}
		items = append(items, numbered{frag: f, index: i, seq: f.Sequence, hasSq: f.HasMfhd})
	}
	// Stable base ordering: declared sequence first (mfhd-less fragments keep
	// manifest position), then original manifest position.
	sort.SliceStable(items, func(a, b int) bool {
		ai, bi := items[a], items[b]
		if ai.hasSq && bi.hasSq && ai.seq != bi.seq {
			return ai.seq < bi.seq
		}
		if ai.hasSq != bi.hasSq {
			return ai.hasSq
		}
		return ai.index < bi.index
	})

	for _, it := range items {
		f := it.frag
		if !it.hasSq {
			out.Issues = append(out.Issues, SequenceIssue{
				Segment: f.Segment, Index: it.index, Kind: "missing",
				Detail:   "fragment has no mfhd; ordered by manifest position",
				Original: it.index,
			})
			out.Fragments = append(out.Fragments, f)
			continue
		}
		if prev, dup := seenSeq[it.seq]; dup {
			prevFrag := out.Fragments[prev]
			if prevFrag == nil || prevFrag.Segment != f.Segment {
				return nil, fault.New(fault.KindSequence, f.Segment, "mfhd", f.MfhdOffset,
					fmt.Sprintf("duplicate sequence number %d conflicts with %s", it.seq, prevFrag.Segment))
			}
			out.Issues = append(out.Issues, SequenceIssue{
				Segment: f.Segment, Sequence: it.seq, Index: it.index, Kind: "duplicate",
				Detail: "byte-identical duplicate dropped", Original: it.index, Duplicate: true,
			})
			continue
		}
		seenSeq[it.seq] = len(out.Fragments)
		out.Fragments = append(out.Fragments, f)
	}

	// Detect gaps between the surviving declared sequence numbers.
	var declared []uint32
	for _, f := range out.Fragments {
		if f.HasMfhd {
			declared = append(declared, f.Sequence)
		}
	}
	for i := 1; i < len(declared); i++ {
		if declared[i] <= declared[i-1] {
			return nil, fault.New(fault.KindSequence, "", "mfhd", 0,
				fmt.Sprintf("declared sequence %d is not strictly increasing after %d", declared[i], declared[i-1]))
		}
		for missing := declared[i-1] + 1; missing < declared[i]; missing++ {
			out.Missing++
			out.Issues = append(out.Issues, SequenceIssue{
				Sequence: missing, Kind: "missing",
				Detail: fmt.Sprintf("mfhd sequence %d absent; gap between %d and %d",
					missing, declared[i-1], declared[i]),
			})
		}
	}

	// Any fragment whose manifest position disagrees with the repaired order
	// is reported, but the repaired order still wins deterministically.
	for pos, f := range out.Fragments {
		if !f.HasMfhd {
			continue
		}
		orig := -1
		for i, cand := range frags {
			if cand == f {
				orig = i
				break
			}
		}
		if orig >= 0 && orig != pos {
			out.Issues = append(out.Issues, SequenceIssue{
				Segment: f.Segment, Sequence: f.Sequence, Index: orig, Kind: "out_of_order",
				Detail: fmt.Sprintf("manifest position %d moved to %d by mfhd order", orig, pos),
			})
		}
	}
	return out, nil
}

// Duplicates counts dropped duplicate fragments.
func (o *Ordered) Duplicates() int {
	n := 0
	for _, is := range o.Issues {
		if is.Kind == "duplicate" && is.Duplicate {
			n++
		}
	}
	return n
}
