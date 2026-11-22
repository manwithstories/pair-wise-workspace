package media

import (
	"fmt"
	"sort"
)

// Range is a locatable span of damaged or recovered input. Ranges are the
// unit of isolation: everything outside them proceeds normally.
type Range struct {
	TrackID     uint32
	TrackKind   string
	Class       Class
	StartNs     int64
	EndNs       int64
	ByteStart   int64
	ByteEnd     int64
	PacketStart int
	PacketEnd   int
	Recoverable bool
	Detail      string
}

// Empty reports whether the range carries no span at all.
func (r Range) Empty() bool {
	return r.StartNs == 0 && r.EndNs == 0 && r.ByteStart == 0 && r.ByteEnd == 0 &&
		r.PacketStart == 0 && r.PacketEnd == 0
}

// Key identifies ranges that may be merged or deduplicated.
func (r Range) Key() string {
	return fmt.Sprintf("%d|%s|%s", r.TrackID, r.TrackKind, r.Class)
}

// MergeRanges sorts and coalesces overlapping ranges that share a track and
// class. The result is deterministic.
func MergeRanges(in []Range) []Range {
	if len(in) == 0 {
		return nil
	}
	out := make([]Range, len(in))
	copy(out, in)
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].TrackID != out[b].TrackID {
			return out[a].TrackID < out[b].TrackID
		}
		if out[a].Class != out[b].Class {
			return out[a].Class < out[b].Class
		}
		if out[a].StartNs != out[b].StartNs {
			return out[a].StartNs < out[b].StartNs
		}
		return out[a].ByteStart < out[b].ByteStart
	})
	merged := out[:1]
	for _, r := range out[1:] {
		last := &merged[len(merged)-1]
		if r.TrackID == last.TrackID && r.Class == last.Class && r.StartNs <= last.EndNs && r.ByteStart <= last.ByteEnd {
			if r.EndNs > last.EndNs {
				last.EndNs = r.EndNs
			}
			if r.ByteEnd > last.ByteEnd {
				last.ByteEnd = r.ByteEnd
			}
			if r.PacketEnd > last.PacketEnd {
				last.PacketEnd = r.PacketEnd
			}
			last.Recoverable = last.Recoverable && r.Recoverable
			continue
		}
		merged = append(merged, r)
	}
	return merged
}
