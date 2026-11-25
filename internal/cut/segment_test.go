package cut

import (
	"testing"

	"mediad/internal/timeline"
)

func au(i int, start int64, key, valid bool) AU {
	return AU{Index: i, StartNS: start, DurationNS: 33_000_000, Keyframe: key, Valid: valid}
}

func TestCutsWaitForKeyframeAfterTarget(t *testing.T) {
	c := New(Config{TargetSegmentNS: 400_000_000})
	inputs := []AU{
		au(0, 0, true, true),
		au(1, 100_000_000, false, true),
		au(2, 450_000_000, false, true), // target reached, cut deferred
		au(3, 600_000_000, true, true),  // deferred cut happens here
	}
	var finalized []Segment
	for _, in := range inputs {
		segs, err := c.Add(in)
		if err != nil {
			t.Fatal(err)
		}
		finalized = append(finalized, segs...)
	}
	if len(finalized) != 1 {
		t.Fatalf("finalized=%d want 1", len(finalized))
	}
	if finalized[0].StartNS != 0 || finalized[0].EndNS != 600_000_000 {
		t.Fatalf("bad boundaries: %+v", finalized[0])
	}
	if finalized[0].KeyframeIndex != 0 || len(finalized[0].AUIndices) != 3 {
		t.Fatalf("bad segment AUs: %+v", finalized[0])
	}
	if c.DeferredCuts != 1 {
		t.Fatalf("deferred=%d want 1", c.DeferredCuts)
	}
	last, err := c.Flush()
	if err != nil || len(last) != 1 {
		t.Fatalf("flush last=%v err=%v", last, err)
	}
	if last[0].KeyframeIndex != 3 || last[0].StartNS != 600_000_000 {
		t.Fatalf("last segment not key aligned: %+v", last[0])
	}
}

func TestNeverStartsOnNonKeyframe(t *testing.T) {
	c := New(Config{})
	for i, start := range []int64{0, 33_000_000, 66_000_000} {
		if _, err := c.Add(au(i, start, false, true)); err != nil {
			t.Fatal(err)
		}
	}
	if c.open != nil {
		t.Fatal("segment should not open before keyframe")
	}
	if _, err := c.Add(au(3, 100_000_000, true, true)); err != nil {
		t.Fatal(err)
	}
	if c.open == nil || c.open.seg.KeyframeIndex != 3 {
		t.Fatalf("expected keyframe start, got %+v", c.open)
	}
}

func TestNoKeyframeForOverTwoSecondsFailsButKeepsCommitted(t *testing.T) {
	c := New(Config{TargetSegmentNS: 500_000_000})
	mustAdd := func(a AU) {
		if _, err := c.Add(a); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	mustAdd(au(0, 0, true, true))
	if segs, err := c.Add(au(1, 1_000_000_000, true, true)); err != nil || len(segs) != 1 {
		t.Fatalf("expected first committed segment, segs=%v err=%v", segs, err)
	}
	mustAdd(au(2, 1_500_000_000, false, true))
	_, err := c.Add(au(3, 3_001_000_000, false, true))
	if err == nil {
		t.Fatal("expected no-keyframe failure")
	}
	if timeline.ExitCode(err) != 3 {
		t.Fatalf("exit code=%d want 3", timeline.ExitCode(err))
	}
	if len(c.Committed) != 1 || c.Committed[0].ID != 0 {
		t.Fatalf("committed segments not retained: %+v", c.Committed)
	}
}

func TestFlushWithoutKeyframeIsUnrecoverable(t *testing.T) {
	c := New(Config{})
	_, err := c.Add(au(0, 0, false, true))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Flush()
	if code := timeline.ExitCode(err); code != 3 {
		t.Fatalf("flush no-key code=%d want 3 err=%v", code, err)
	}
}

func TestBadAUMarksSegmentButDoesNotPanic(t *testing.T) {
	c := New(Config{})
	if _, err := c.Add(au(0, 0, true, true)); err != nil {
		t.Fatal(err)
	}
	bad := au(1, 33_000_000, false, false)
	if _, err := c.Add(bad); err != nil {
		t.Fatal(err)
	}
	if !c.open.seg.ContainsBadAUs {
		t.Fatal("segment should record isolated bad AU")
	}
}
