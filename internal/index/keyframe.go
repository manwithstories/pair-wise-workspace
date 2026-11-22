// Package index builds keyframe indexes from probed fragments and rebuilds
// them by scanning segments when an index is incomplete. Entries whose
// keyframe status cannot be established are reported but excluded from the
// hit-rate denominator.
package index

import (
	"sort"

	"mediad/internal/clock"
	"mediad/internal/fault"
	"mediad/internal/probe"
)

// Entry is one keyframe or an undetermined candidate.
type Entry struct {
	Segment          string `json:"segment"`
	RepresentationID string `json:"representation_id,omitempty"`
	Sequence         uint64 `json:"sequence"`
	Start            int64  `json:"start"`    // canonical ticks
	Duration         int64  `json:"duration"` // canonical ticks, 0 if unknown
	Keyframe         bool   `json:"keyframe"`
	Determined       bool   `json:"determined"`
	Source           string `json:"source"` // "index" or "scan"
}

// Index is the resolved keyframe timeline.
type Index struct {
	Entries    []Entry       `json:"entries"`
	Incomplete bool          `json:"incomplete"`
	Rebuilt    bool          `json:"rebuilt"`
	Faults     []fault.Fault `json:"faults,omitempty"`
}

// Stats reports the keyframe hit rate over determinable entries.
func (i *Index) Stats() (hits, determinable, undetermined int) {
	for _, e := range i.Entries {
		if !e.Determined {
			undetermined++
			continue
		}
		determinable++
		if e.Keyframe {
			hits++
		}
	}
	return hits, determinable, undetermined
}

// HitRate returns hits/determinable as a percentage, or 0 when none are
// determinable.
func (i *Index) HitRate() float64 {
	hits, det, _ := i.Stats()
	if det == 0 {
		return 0
	}
	return 100 * float64(hits) / float64(det)
}

// Build assembles entries from probed media segments. A segment lacking a
// usable timeline contributes an undetermined entry and marks the index
// incomplete; no duration is invented for it.
func Build(uri string, seq uint64, rep string, info *probe.MediaInfo) []Entry {
	if info == nil {
		return []Entry{{
			Segment: uri, RepresentationID: rep, Sequence: seq,
			Determined: false, Source: "scan",
		}}
	}
	if !info.Determined || len(info.Fragments) == 0 {
		return []Entry{{
			Segment: uri, RepresentationID: rep, Sequence: seq,
			Determined: false, Source: "scan",
		}}
	}
	var out []Entry
	for _, frag := range info.Fragments {
		start := frag.StartCanonical()
		if !frag.Determined || !frag.TimescaleValid() {
			out = append(out, Entry{
				Segment: uri, RepresentationID: rep, Sequence: seq,
				Start: start, Determined: false, Source: "scan",
			})
			continue
		}
		native := clock.Timebase{Timescale: frag.Timescale}
		pos := start
		for _, s := range frag.Samples {
			dur := native.Convert(int64(s.Duration), clock.Default())
			out = append(out, Entry{
				Segment: uri, RepresentationID: rep, Sequence: seq,
				Start: pos, Duration: dur, Keyframe: s.Sync,
				Determined: true, Source: "scan",
			})
			pos += dur
		}
	}
	if len(out) == 0 {
		out = append(out, Entry{Segment: uri, RepresentationID: rep, Sequence: seq, Determined: false, Source: "scan"})
	}
	return out
}

// Merge combines entries from several sources, de-duplicating on
// (segment, start) and preferring determined entries. The result is sorted
// for deterministic output.
func Merge(lists ...[]Entry) []Entry {
	type key struct {
		seg   string
		start int64
	}
	seen := map[key]int{}
	var out []Entry
	for _, list := range lists {
		for _, e := range list {
			k := key{e.Segment, e.Start}
			if idx, ok := seen[k]; ok {
				if !out[idx].Determined && e.Determined {
					out[idx] = e
				}
				continue
			}
			seen[k] = len(out)
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Segment != out[j].Segment {
			return out[i].Segment < out[j].Segment
		}
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].Sequence < out[j].Sequence
	})
	return out
}

// Rebuild scans media segment probes to regenerate a complete index. It is
// used when a shipped index is missing or partial; segments that cannot be
// scanned remain undetermined and are reported.
func Rebuild(segments []SegmentRef, scan func(SegmentRef) (*probe.MediaInfo, *fault.List)) *Index {
	idx := &Index{Rebuilt: true, Incomplete: true}
	for _, ref := range segments {
		info, fl := scan(ref)
		if fl != nil {
			idx.Faults = append(idx.Faults, fl.All()...)
		}
		entries := Build(ref.URI, ref.Sequence, ref.RepresentationID, info)
		idx.Entries = append(idx.Entries, entries...)
		for _, e := range entries {
			if !e.Determined {
				idx.Incomplete = true
			}
		}
	}
	idx.Entries = Merge(idx.Entries)
	idx.Incomplete = false
	for _, e := range idx.Entries {
		if !e.Determined {
			idx.Incomplete = true
			break
		}
	}
	return idx
}

// SegmentRef identifies a segment to scan during a rebuild.
type SegmentRef struct {
	URI              string
	RepresentationID string
	Sequence         uint64
}

// SeekFirst returns the last determined keyframe at or before t.
func (i *Index) SeekFirst(t int64) (Entry, bool) {
	var best Entry
	found := false
	for _, e := range i.Entries {
		if !e.Determined || !e.Keyframe {
			continue
		}
		if e.Start <= t && (!found || e.Start >= best.Start) {
			best, found = e, true
		}
	}
	return best, found
}

// Keyframes returns determined keyframe entries in timeline order.
func (i *Index) Keyframes() []Entry {
	var out []Entry
	for _, e := range i.Entries {
		if e.Determined && e.Keyframe {
			out = append(out, e)
		}
	}
	return out
}
