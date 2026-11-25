package segment_test

import (
	"testing"

	"mediaproc/internal/demux"
	"mediaproc/internal/model"
	"mediaproc/internal/segment"
)

func block(track uint64, ms int64, key bool, payload ...byte) *demux.Block {
	return &demux.Block{
		TrackNumber: track,
		Offset:      100 + ms,
		TimeNS:      model.Nanoseconds(ms) * model.Millisecond,
		Keyframe:    key,
		Frames:      [][]byte{payload},
		Data:        payload,
	}
}

func TestCutsAtKeyframesAndRanges(t *testing.T) {
	c := segment.NewCutter(segment.DefaultOptions())
	var closed []*segment.Segment

	mustAdd := func(b *demux.Block) {
		if s := c.Add(b); s != nil {
			closed = append(closed, s)
		}
	}

	mustAdd(block(1, 0, true, 1))
	mustAdd(block(2, 20, false, 2))
	mustAdd(block(1, 200, true, 3)) // closes segment 0
	mustAdd(block(2, 220, false, 4))
	last := c.Flush()
	if last == nil {
		t.Fatal("expected final segment")
	}

	if len(closed) != 1 {
		t.Fatalf("closed=%d", len(closed))
	}
	s0 := closed[0]
	if s0.Index != 0 || s0.KeyframeIndex != 0 || s0.StartNS.Milliseconds() != 0 ||
		s0.EndNS.Milliseconds() != 200 {
		t.Fatalf("segment0 range/index wrong: %+v", s0)
	}
	if len(s0.Blocks) != 2 {
		t.Fatalf("segment0 blocks=%d", len(s0.Blocks))
	}
	if last.Index != 1 || last.KeyframeIndex != 1 || last.StartNS.Milliseconds() != 200 {
		t.Fatalf("segment1 wrong: %+v", last)
	}
	// Last timestamp 220ms plus default 20ms tail.
	if last.EndNS.Milliseconds() != 240 {
		t.Fatalf("last end=%d want 240", last.EndNS.Milliseconds())
	}
}

func TestHashIsDeterministicAndSensitive(t *testing.T) {
	makeSeg := func(payload byte) *segment.Segment {
		c := segment.NewCutter(segment.DefaultOptions())
		c.Add(block(1, 0, true, payload))
		s := c.Flush()
		s.SHA256 = segment.Hash(s)
		return s
	}
	a := makeSeg(0xaa)
	b := makeSeg(0xaa)
	if a.SHA256 == "" || a.SHA256 != b.SHA256 {
		t.Fatalf("deterministic hash failed: %q vs %q", a.SHA256, b.SHA256)
	}
	changed := makeSeg(0xbb)
	if a.SHA256 == changed.SHA256 {
		t.Fatal("hash did not change with payload")
	}
	if err := verifyHash(a); err != nil {
		t.Fatal(err)
	}
}

func verifyHash(s *segment.Segment) error {
	if got := segment.Hash(s); got != s.SHA256 {
		return &hashError{got: got, want: s.SHA256}
	}
	return nil
}

type hashError struct{ got, want string }

func (e *hashError) Error() string { return "hash mismatch" }
