package sync

import (
	"strings"
	"testing"

	"mediarun/internal/buffer"
	"mediarun/internal/clock"
)

func TestAlignConstantOffset(t *testing.T) {
	a := New("audio", DefaultConfig())
	// Audio lags video by a steady 12ms across 2 seconds.
	for i := 0; i < 20; i++ {
		ns := int64(i) * 100_000_000
		a.Add(Anchor{AudioNS: ns, VideoNS: ns + 12_000_000})
	}
	c, err := a.CorrectInterval(0, 2_000_000_000)
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if c.Applied != 12_000_000 {
		t.Fatalf("applied = %d, want 12ms", c.Applied)
	}
	if c.Rejected {
		t.Fatalf("should not be rejected")
	}
}

func TestAlignRejectsBeyondThreshold(t *testing.T) {
	a := New("audio", DefaultConfig())
	for i := 0; i < 20; i++ {
		ns := int64(i) * 100_000_000
		a.Add(Anchor{AudioNS: ns, VideoNS: ns + 400_000_000}) // 400ms
	}
	c, err := a.CorrectInterval(0, 2_000_000_000)
	if err == nil {
		t.Fatalf("400ms correction must be rejected")
	}
	if !c.Rejected {
		t.Fatalf("correction not flagged: %+v", c)
	}
	if !strings.Contains(err.Error(), "sync") {
		t.Fatalf("wrong error: %v", err)
	}
	var e *clock.Error
	if !asClock(err, &e) || e.Kind != clock.KindUnrecoverable {
		t.Fatalf("wrong kind: %v", err)
	}
}

func TestAlignInsufficientInput(t *testing.T) {
	a := New("audio", DefaultConfig())
	a.Add(Anchor{AudioNS: 0, VideoNS: 0})
	a.Add(Anchor{AudioNS: 1_000_000, VideoNS: 1_000_000})
	if _, err := a.CorrectInterval(0, 2_000_000_000); err == nil {
		t.Fatalf("two anchors must be insufficient")
	}
}

func TestAlignMedianRobustToOneBadAnchor(t *testing.T) {
	a := New("audio", DefaultConfig())
	for i := 0; i < 10; i++ {
		ns := int64(i) * 100_000_000
		off := int64(5_000_000)
		if i == 5 {
			off = 900_000_000 // one wildly mistimed anchor
		}
		a.Add(Anchor{AudioNS: ns, VideoNS: ns + off})
	}
	c, err := a.CorrectInterval(0, 1_000_000_000)
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if c.Applied != 5_000_000 {
		t.Fatalf("median = %d, want 5ms", c.Applied)
	}
}

func TestAlignIntervalsStopsBadIntervals(t *testing.T) {
	a := New("audio", DefaultConfig())
	for i := 0; i < 4; i++ {
		ns := int64(i) * 100_000_000
		a.Add(Anchor{AudioNS: ns, VideoNS: ns + 10_000_000})
	}
	// Second interval has no anchors at all.
	ivs := []buffer.Interval{
		{Track: "audio", StartNS: 0, EndNS: 400_000_000},
		{Track: "audio", StartNS: 500_000_000, EndNS: 900_000_000},
	}
	got, errs := AlignIntervals(a, ivs)
	if len(got) != 1 || len(errs) != 1 {
		t.Fatalf("got=%d errs=%d", len(got), len(errs))
	}
	if !clock.IsKind(errs[0], clock.KindUnrecoverable) {
		t.Fatalf("wrong kind: %v", errs[0])
	}
}

func asClock(err error, target **clock.Error) bool {
	if e, ok := err.(*clock.Error); ok {
		*target = e
		return true
	}
	return false
}
