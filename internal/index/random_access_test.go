package index

import (
	"testing"

	"github.com/pairwise/mediad/internal/media"
)

func testEntries() []Entry {
	return []Entry{
		{Index: 0, SegmentID: "s0", StartTicks: 0, EndTicks: 180000, Keyframe: true, Complete: true, Size: 10},
		{Index: 1, SegmentID: "s1", StartTicks: 180000, EndTicks: 360000, Keyframe: true, Complete: true, Size: 11},
		{Index: 2, SegmentID: "s2", StartTicks: 360000, EndTicks: 540000, Keyframe: true, Complete: true, Size: 12},
	}
}

func TestRandomAccessQueries(t *testing.T) {
	idx, err := New(testEntries())
	if err != nil {
		t.Fatal(err)
	}
	e, err := idx.AtTime(200000)
	if err != nil || e.Index != 1 {
		t.Fatalf("entry=%+v err=%v", e, err)
	}
	k, err := idx.SeekKeyframe(370000)
	if err != nil || k.Index != 2 {
		t.Fatalf("keyframe=%+v err=%v", k, err)
	}
	queries := []int64{0, 10, 180000, 200000, 360000, 539999}
	hits, total, rate, err := idx.KeyframeHitRate(queries)
	if err != nil || hits != total || rate != 100 {
		t.Fatalf("hits=%d/%d rate=%f err=%v", hits, total, rate, err)
	}
}

func TestOutOfRangeStructuredError(t *testing.T) {
	idx, _ := New(testEntries())
	if _, err := idx.AtTime(999999); media.ExitCode(err) != 4 {
		t.Fatalf("AtTime err=%v", err)
	}
	_, err := idx.Segment(99)
	me, ok := media.AsError(err)
	if !ok || me.Code != media.CodeOutOfRange || me.Field != "segment_index" {
		t.Fatalf("err=%v", err)
	}
}

func TestIncompleteSegmentCannotBeKeyframe(t *testing.T) {
	entries := testEntries()
	entries[1].Complete = false
	_, err := New(entries)
	if me, ok := media.AsError(err); !ok || me.Code != media.CodeMalformed {
		t.Fatalf("err=%v", err)
	}
}
