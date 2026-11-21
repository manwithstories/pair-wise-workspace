package adapter

import (
	"context"
	"testing"
)

func TestGeneratedMXFRoundTrips(t *testing.T) {
	layout, err := BuildMXF(MXFGenerator{Tracks: 3, Samples: 25, UnknownKLV: 2})
	if err != nil {
		t.Fatal(err)
	}
	src := NewMixedSource(layout, nil, "probe.mxf")
	c, err := (&MXFAdapter{}).Parse(context.Background(), src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(c.Tracks))
	}
	if c.UnknownBoxes != 2 {
		t.Fatalf("unknown=%d", c.UnknownBoxes)
	}
	for _, tr := range c.Tracks {
		if tr.SampleCount() != 25 {
			t.Fatalf("track %s samples=%d", tr.ID, tr.SampleCount())
		}
		s, err := tr.Index.At(3)
		if err != nil {
			t.Fatal(err)
		}
		want := layout.SampleOffsets[tr.ID.Value][3]
		if s.Offset != want || s.Size != int64(layout.SampleSizes[tr.ID.Value][3]) {
			t.Fatalf("track %s sample 3 off=%d want %d size=%d", tr.ID, s.Offset, want, s.Size)
		}
	}
}
