// Package fault defines the shared error model: every detected corruption
// carries a precise position (track, sample, byte offset) and the chain of
// pipeline nodes it blocks, so damage stays traceable end to end.
package fault

import "fmt"

// Kind classifies a detected fault.
type Kind string

const (
	KindTruncatedSampleTable Kind = "truncated_sample_table"
	KindOffsetOutOfRange     Kind = "offset_out_of_range"
	KindEditListOutOfRange   Kind = "edit_list_out_of_range"
	KindEditListOverlap      Kind = "edit_list_overlap"
	KindDuplicateTimestamp   Kind = "duplicate_timestamp"
	KindTimestampOverflow    Kind = "timestamp_overflow"
	KindUnsupportedLayout    Kind = "unsupported_layout"
	KindColorConflict        Kind = "color_conflict"
	KindMissingKeyframe      Kind = "missing_keyframe"
	KindLimitExceeded        Kind = "limit_exceeded"
)

// Fault is one located defect in an input asset.
type Fault struct {
	Kind        Kind     `json:"kind"`
	Track       int      `json:"track"`       // 1-based track number; 0 = file-level
	Sample      int64    `json:"sample"`      // first affected sample; -1 if n/a
	Offset      int64    `json:"offset"`      // byte offset in file; -1 if n/a
	Chain       []string `json:"chain"`       // affected nodes, Chain[0] = first blocked dependency
	Recoverable bool     `json:"recoverable"` // true if the pipeline may continue on the recoverable range
	Message     string   `json:"message"`
}

func (f *Fault) Error() string {
	return fmt.Sprintf("fault[%s] track=%d sample=%d offset=%d: %s",
		f.Kind, f.Track, f.Sample, f.Offset, f.Message)
}

// FirstBlocked names the first pipeline node blocked by this fault.
func (f *Fault) FirstBlocked() string {
	if len(f.Chain) == 0 {
		return ""
	}
	return f.Chain[0]
}

// List is an ordered set of faults (detection order).
type List []*Fault

// First returns the earliest fault, or nil.
func (l List) First() *Fault {
	if len(l) == 0 {
		return nil
	}
	return l[0]
}

// Blocking returns faults that cannot be recovered from.
func (l List) Blocking() List {
	var out List
	for _, f := range l {
		if !f.Recoverable {
			out = append(out, f)
		}
	}
	return out
}

// FirstBlockedName is the first blocked dependency across all faults.
func (l List) FirstBlockedName() string {
	for _, f := range l {
		if n := f.FirstBlocked(); n != "" {
			return n
		}
	}
	return ""
}
