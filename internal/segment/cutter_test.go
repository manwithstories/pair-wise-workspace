package segment

import (
	"testing"

	"mediaprobe/internal/mp4"
)

func ts(pts, dur int64, key bool) TimedSample {
	return TimedSample{
		Sample:   mp4.Sample{Keyframe: key, TrackID: 1},
		MediaPTS: pts, MediaDur: dur,
	}
}

// feed builds a stream with keyframes every keyEvery samples of dur each.
func feed(c *Cutter, n, keyEvery int, dur int64) {
	for i := 0; i < n; i++ {
		c.Add(ts(int64(i)*dur, dur, i%keyEvery == 0))
	}
	c.Flush()
}

func TestCutAtKeyframeBoundaries(t *testing.T) {
	c := NewCutter(2000) // target: 2 samples of 1000
	feed(c, 10, 1, 1000)
	segs := c.Segments()
	if len(segs) != 5 {
		t.Fatalf("segments=%d", len(segs))
	}
	for i, s := range segs {
		if s.Start != int64(i)*2000 {
			t.Fatalf("seg %d start=%d", i, s.Start)
		}
		if !s.Samples[0].Keyframe {
			t.Fatalf("seg %d does not start at a keyframe", i)
		}
	}
	for _, cut := range c.Cuts() {
		if cut.Deviation != 0 {
			t.Fatalf("cut=%+v", cut)
		}
	}
}

func TestSparseKeyframesRecordDeviation(t *testing.T) {
	c := NewCutter(2000)
	feed(c, 12, 3, 1000) // keyframes at 0,3000,6000,9000
	segs := c.Segments()
	if len(segs) != 4 {
		t.Fatalf("segments=%d", len(segs))
	}
	// Targets roll with actual cuts: 2000->3000, 5000->6000, 8000->9000.
	wantDev := []int64{1000, 1000, 1000}
	for i, cut := range c.Cuts()[:3] {
		if cut.Deviation != wantDev[i] {
			t.Fatalf("cut %d deviation=%d want %d", i, cut.Deviation, wantDev[i])
		}
		if cut.Actual-cut.Target != cut.Deviation {
			t.Fatalf("cut %d inconsistent: %+v", i, cut)
		}
	}
}

func TestVFRTimestamps(t *testing.T) {
	c := NewCutter(5000)
	// VFR: alternating 3000/1500 durations.
	var pts int64
	for i := 0; i < 8; i++ {
		dur := int64(3000)
		if i%2 == 1 {
			dur = 1500
		}
		c.Add(ts(pts, dur, i%2 == 0))
		pts += dur
	}
	c.Flush()
	for _, s := range c.Segments() {
		if !s.Samples[0].Keyframe {
			t.Fatal("segment not starting at keyframe")
		}
		if s.End <= s.Start {
			t.Fatalf("bad span %+v", s)
		}
	}
}

func TestUncertainPropagatesToSegment(t *testing.T) {
	c := NewCutter(1000)
	s := ts(0, 1000, true)
	s.Uncertain = true
	c.Add(s)
	c.Add(ts(1000, 1000, true))
	c.Flush()
	if !c.Segments()[0].Uncertain {
		t.Fatal("uncertain sample did not taint its segment")
	}
}
