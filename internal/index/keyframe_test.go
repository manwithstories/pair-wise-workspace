package index

import (
	"strings"
	"testing"

	"mediad/internal/fault"
	"mediad/internal/probe"
)

func mediaInfo(start, dur int64, syncs ...bool) *probe.MediaInfo {
	frag := probe.Fragment{
		Sequence: 1, TrackID: 1, BaseTime: uint64(start), Timescale: 90000, Determined: true,
	}
	for _, s := range syncs {
		flags := uint32(0x01010000)
		if s {
			flags = 0x02000000
		}
		frag.Samples = append(frag.Samples, probe.Sample{Duration: uint32(dur), Size: 10, Flags: flags, Sync: s})
		frag.Duration += uint32(dur)
	}
	return &probe.MediaInfo{
		URI: "s.m4s", Size: 100, Fragments: []probe.Fragment{frag},
		HasMoof: true, HasMdat: true, Determined: true,
		StartTicks: start, EndTicks: start + dur*int64(len(syncs)),
	}
}

func TestBuildAndStats(t *testing.T) {
	info := mediaInfo(0, 9000, true, false, false, true)
	entries := Build("s1.m4s", 1, "rep", info)
	if len(entries) != 4 {
		t.Fatalf("entries = %d", len(entries))
	}
	idx := &Index{Entries: entries}
	hits, det, undet := idx.Stats()
	if hits != 2 || det != 4 || undet != 0 {
		t.Fatalf("stats = %d/%d/%d", hits, det, undet)
	}
	if idx.HitRate() != 50 {
		t.Fatalf("hit rate = %v", idx.HitRate())
	}
	if idx.Entries[3].Start != 27000 {
		t.Fatalf("start = %d", idx.Entries[3].Start)
	}
}

func TestUndeterminedExcludedFromDenominator(t *testing.T) {
	idx := &Index{Entries: []Entry{
		{Segment: "a", Start: 0, Keyframe: true, Determined: true},
		{Segment: "b", Determined: false},
		{Segment: "c", Determined: false},
	}}
	hits, det, undet := idx.Stats()
	if hits != 1 || det != 1 || undet != 2 {
		t.Fatalf("stats = %d/%d/%d", hits, det, undet)
	}
	if idx.HitRate() != 100 {
		t.Fatalf("hit rate = %v", idx.HitRate())
	}
	if (&Index{}).HitRate() != 0 {
		t.Fatal("empty index rate must be 0")
	}
}

func TestBuildUndeterminedForBadProbe(t *testing.T) {
	entries := Build("bad.m4s", 2, "rep", nil)
	if len(entries) != 1 || entries[0].Determined {
		t.Fatalf("entries = %+v", entries)
	}
	bad := mediaInfo(0, 9000, true)
	bad.Determined = false
	entries = Build("bad2.m4s", 3, "rep", bad)
	if len(entries) != 1 || entries[0].Determined || entries[0].Duration != 0 {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestMergePrefersDetermined(t *testing.T) {
	a := []Entry{{Segment: "a", Start: 0, Determined: false}, {Segment: "a", Start: 10, Determined: true}}
	b := []Entry{{Segment: "a", Start: 0, Keyframe: true, Determined: true}}
	merged := Merge(a, b)
	if len(merged) != 2 {
		t.Fatalf("merged = %+v", merged)
	}
	if !merged[0].Determined || !merged[0].Keyframe {
		t.Fatalf("determined entry not preferred: %+v", merged[0])
	}
}

func TestRebuildScansSegments(t *testing.T) {
	refs := []SegmentRef{
		{URI: "a.m4s", Sequence: 1}, {URI: "b.m4s", Sequence: 2},
	}
	idx := Rebuild(refs, func(ref SegmentRef) (*probe.MediaInfo, *fault.List) {
		if ref.URI == "b.m4s" {
			var fl fault.List
			fl.Add(fault.MustNew(fault.KindMissingMdat, fault.Location{Segment: ref.URI, Offset: 4}, "no mdat", true, false))
			return nil, &fl
		}
		return mediaInfo(0, 9000, true, false), nil
	})
	if !idx.Rebuilt || !idx.Incomplete {
		t.Fatalf("index flags = %+v", idx)
	}
	if len(idx.Faults) != 1 || idx.Faults[0].Kind != fault.KindMissingMdat {
		t.Fatalf("faults = %+v", idx.Faults)
	}
	hits, det, undet := idx.Stats()
	if hits != 1 || det != 2 || undet != 1 {
		t.Fatalf("stats = %d/%d/%d", hits, det, undet)
	}
}

func TestSeekFirst(t *testing.T) {
	idx := &Index{Entries: []Entry{
		{Segment: "a", Start: 0, Keyframe: true, Determined: true},
		{Segment: "a", Start: 9000, Determined: true},
		{Segment: "b", Start: 18000, Keyframe: true, Determined: true},
	}}
	e, ok := idx.SeekFirst(9000)
	if !ok || e.Start != 0 {
		t.Fatalf("seek = %+v %v", e, ok)
	}
	e, ok = idx.SeekFirst(18000)
	if !ok || e.Start != 18000 {
		t.Fatalf("seek = %+v %v", e, ok)
	}
	if _, ok := idx.SeekFirst(-1); ok {
		t.Fatal("no keyframe before zero")
	}
	if got := idx.Keyframes(); len(got) != 2 || !strings.Contains(got[0].Segment, "a") {
		t.Fatalf("keyframes = %+v", got)
	}
}
