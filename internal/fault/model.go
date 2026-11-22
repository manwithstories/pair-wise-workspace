// Package fault defines the cross-subsystem defect taxonomy and the
// media-time interval type used to locate every defect on the same clock.
package fault

import (
	"errors"
	"fmt"
)

// Category classifies a defect independently of the subsystem that found it.
type Category string

const (
	CatBadChecksum  Category = "bad_checksum"   // frame SHA-256 mismatch
	CatTruncatedBox Category = "truncated_box"  // MP4 box payload shorter than declared
	CatIllegalSize  Category = "illegal_size"   // box/frame size invalid or over limit
	CatDupTimestamp Category = "duplicate_timestamp"
	CatNonMonotonic Category = "non_monotonic_ts"
	CatSizeViolation Category = "frame_size_violation"
	CatRTPDuplicate Category = "rtp_duplicate"  // repeated extended sequence number
	CatRTPLate      Category = "rtp_late"       // arrived past the reorder window
	CatBurstLoss    Category = "burst_loss"     // consecutive missing sequence numbers
	CatGOPAnomaly   Category = "gop_anomaly"    // keyframe interval outside tolerance
)

// ErrFatal marks an input/manifest integrity defect (exit code 2).
var ErrFatal = errors.New("fault: fatal input or manifest defect")

// Interval is a half-open media-time interval [Start, End) in clock ticks.
type Interval struct {
	Start int64
	End   int64
}

// Validate rejects inverted intervals.
func (i Interval) Validate() error {
	if i.End < i.Start {
		return fmt.Errorf("fault: inverted interval [%d,%d)", i.Start, i.End)
	}
	return nil
}

// Overlaps reports whether two intervals share any tick.
func (i Interval) Overlaps(o Interval) bool {
	return i.Start < o.End && o.Start < i.End
}

// Merge unions overlapping intervals or intervals separated by at most gap ticks.
func Merge(in []Interval, gap int64) ([]Interval, error) {
	if gap < 0 {
		return nil, fmt.Errorf("fault: negative merge gap %d", gap)
	}
	for _, iv := range in {
		if err := iv.Validate(); err != nil {
			return nil, err
		}
	}
	if len(in) < 2 {
		return append([]Interval(nil), in...), nil
	}
	out := make([]Interval, 0, len(in))
	cur := in[0]
	for _, iv := range in[1:] {
		if iv.Start <= cur.End+gap {
			if iv.End > cur.End {
				cur.End = iv.End
			}
			continue
		}
		out = append(out, cur)
		cur = iv
	}
	out = append(out, cur)
	return out, nil
}

// Fault is one located defect. Byte is the file offset for container
// defects (-1 when not applicable); Span locates the defect on the media
// timeline (zero interval when not applicable).
type Fault struct {
	Cat Category
	// Byte is the absolute file byte offset, or -1.
	Byte int64
	// Span is the media-time range in clock ticks.
	Span Interval
	// Ref is the offending object id (frame id, box type, rtp seq).
	Ref string
	Msg string
}

// Fatal reports whether the defect makes the input itself invalid,
// as opposed to being a recoverable transport-layer condition.
func (f Fault) Fatal() bool {
	switch f.Cat {
	case CatBadChecksum, CatTruncatedBox, CatIllegalSize,
		CatDupTimestamp, CatNonMonotonic, CatSizeViolation:
		return true
	default:
		return false
	}
}

func (f Fault) Error() string {
	switch {
	case f.Byte >= 0:
		return fmt.Sprintf("%s at byte %d (ref=%s): %s", f.Cat, f.Byte, f.Ref, f.Msg)
	case f.Span.End > f.Span.Start:
		return fmt.Sprintf("%s at ticks [%d,%d) (ref=%s): %s", f.Cat, f.Span.Start, f.Span.End, f.Ref, f.Msg)
	default:
		return fmt.Sprintf("%s (ref=%s): %s", f.Cat, f.Ref, f.Msg)
	}
}

// FatalCount returns how many defects are input-invalid.
func FatalCount(fs []Fault) int {
	n := 0
	for _, f := range fs {
		if f.Fatal() {
			n++
		}
	}
	return n
}
