package fingerprint

import (
	"testing"

	"mediaprobe/internal/mp4"
	"mediaprobe/internal/segment"
)

func seg(id int, start int64, uncertain bool, data ...[]byte) *segment.Segment {
	s := &segment.Segment{ID: id, Start: start, Uncertain: uncertain}
	for _, d := range data {
		s.Samples = append(s.Samples, segment.TimedSample{
			Sample: mp4.Sample{Data: d, Size: uint32(len(d))},
		})
	}
	return s
}

func TestHashIgnoresTimestamps(t *testing.T) {
	a := seg(0, 0, false, []byte("hello"), []byte("world"))
	b := seg(1, 987654, false, []byte("hello"), []byte("world"))
	if Hash(a) != Hash(b) {
		t.Fatal("identical content at different times must hash equal")
	}
}

func TestHashCoversBoundarySamples(t *testing.T) {
	a := seg(0, 0, false, []byte("abc"))
	b := seg(1, 0, false, []byte("abd"))
	if Hash(a) == Hash(b) {
		t.Fatal("different boundary sample content must not collide")
	}
}

func TestDeduperFindsCrossFragmentRepeats(t *testing.T) {
	d := NewDeduper()
	if _, dup := d.Add(seg(0, 0, false, []byte("x"))); dup {
		t.Fatal("first segment flagged as dup")
	}
	if canon, dup := d.Add(seg(1, 5000, false, []byte("y"))); dup || canon != 1 {
		t.Fatal("distinct content flagged as dup")
	}
	canon, dup := d.Add(seg(2, 9000, false, []byte("x")))
	if !dup || canon != 0 {
		t.Fatalf("dup=%v canon=%d", dup, canon)
	}
	if len(d.Duplicates()) != 1 || d.Duplicates()[0] != 2 {
		t.Fatalf("dups=%v", d.Duplicates())
	}
}

func TestUncertainSegmentsDedupByContentOnly(t *testing.T) {
	d := NewDeduper()
	d.Add(seg(0, 100, true, []byte("data")))
	// Same content, different (uncertain) timing: content decides.
	if canon, dup := d.Add(seg(1, 200, true, []byte("data"))); !dup || canon != 0 {
		t.Fatalf("dup=%v canon=%d", dup, canon)
	}
	// Same timestamp, different content: must NOT be judged by time alone.
	if _, dup := d.Add(seg(2, 100, true, []byte("other"))); dup {
		t.Fatal("timestamp match with different content must not dedup")
	}
}
