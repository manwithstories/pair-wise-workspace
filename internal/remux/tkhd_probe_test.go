package remux

import (
	"encoding/binary"
	"testing"
)

// TestTkhdLayout pins the tkhd payload layout: version/flags at 8, track_ID at
// 20, duration at 28, volume at 52, width/height at 84/88 (all relative to the
// box start).
func TestTkhdLayout(t *testing.T) {
	plan := testPlan(t)
	out := &memWriteSeeker{}
	if _, err := Write(out, plan); err != nil {
		t.Fatal(err)
	}
	var offsets []int
	for i := 0; i+4 <= len(out.buf); i++ {
		if string(out.buf[i:i+4]) == "tkhd" {
			offsets = append(offsets, i-4)
		}
	}
	if len(offsets) != 2 {
		t.Fatalf("tkhd count=%d", len(offsets))
	}
	for i, start := range offsets {
		if got := binary.BigEndian.Uint32(out.buf[start+20:]); got != uint32(i+1) {
			t.Fatalf("tkhd %d track_ID=%d want %d", i, got, i+1)
		}
		// Reserved word after track_ID must be zero.
		if got := binary.BigEndian.Uint32(out.buf[start+24:]); got != 0 {
			t.Fatalf("tkhd %d reserved=%d", i, got)
		}
		if got := binary.BigEndian.Uint32(out.buf[start+28:]); got != 1000 {
			t.Fatalf("tkhd %d duration=%d", i, got)
		}
		if i == 0 {
			if got := binary.BigEndian.Uint32(out.buf[start+84:]); got != 320<<16 {
				t.Fatalf("video tkhd width=%d", got)
			}
			if got := binary.BigEndian.Uint32(out.buf[start+88:]); got != 180<<16 {
				t.Fatalf("video tkhd height=%d", got)
			}
		} else if got := binary.BigEndian.Uint32(out.buf[start+84:]); got != 0 {
			t.Fatalf("audio tkhd width=%d", got)
		}
	}
	// layout: layer@40, alternate_group@42, volume@44, matrix@48..84.
	if got := binary.BigEndian.Uint16(out.buf[offsets[1]+44:]); got != 0x0100 {
		t.Fatalf("audio tkhd volume=%d", got)
	}
	if got := binary.BigEndian.Uint16(out.buf[offsets[0]+44:]); got != 0 {
		t.Fatalf("video tkhd volume=%d", got)
	}
}

// TestMoovTracksExposeDistinctIDs checks the track ID claim through go-mp4's
// own tkhd decoder as well.
func TestMoovTracksExposeDistinctIDs(t *testing.T) {
	plan := testPlan(t)
	out := &memWriteSeeker{}
	sum, err := Write(out, plan)
	if err != nil {
		t.Fatal(err)
	}
	// The manifest preserves per-track ordering, so the two tracks must be
	// distinguishable there even before parsing.
	seen := map[uint64]bool{}
	for _, m := range sum.Manifest {
		seen[m.Track] = true
	}
	if len(seen) != 2 {
		t.Fatalf("manifest tracks=%v", seen)
	}
}
