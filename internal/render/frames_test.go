package render

import (
	"bytes"
	"testing"

	"mediasync/internal/timeline"
)

func TestFramesDeterministic(t *testing.T) {
	run := func() (string, int, string) {
		g := NewGenerator()
		var buf bytes.Buffer
		evs := []timeline.Event{
			{Track: "subtitle", Start: 0, End: 2_500_000_000, Label: "a"},
			{Track: "timecode", Start: 1_000_000_000, End: 1_000_000_000, Label: "m"},
		}
		if err := g.Advance(3*Interval, evs, &buf); err != nil {
			t.Fatal(err)
		}
		return g.Summary(), g.Count, buf.String()
	}
	s1, c1, o1 := run()
	s2, c2, o2 := run()
	if s1 != s2 || c1 != c2 || o1 != o2 {
		t.Fatal("frame generation not deterministic")
	}
	if c1 != 3 {
		t.Fatalf("count = %d, want 3", c1)
	}
}

func TestFrameCountMatchesInterval(t *testing.T) {
	g := NewGenerator()
	if err := g.Advance(10*Interval, nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if g.Count != 10 {
		t.Fatalf("count = %d, want 10", g.Count)
	}
}
