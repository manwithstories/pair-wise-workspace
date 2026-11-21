package adapter

import (
	"context"
	"testing"
)

func TestGeneratedMP4RoundTrips(t *testing.T) {
	layout, err := BuildMP4(MP4Spec{
		Tracks: 3, Samples: 40, Seed: 7,
		Color: &MediaColor{Primaries: 9, Transfer: 16, Matrix: 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	src := NewLogicalSource(layout, "probe.mp4")
	c, err := (&MP4Adapter{}).Parse(context.Background(), src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(c.Tracks))
	}
	for _, tr := range c.Tracks {
		// Tracks are reported in deterministic sorted order, so map back to
		// the generator's track ordinal through the stable identifier.
		ti := int(tr.ID.Value) - 1
		if tr.SampleCount() != 40 {
			t.Fatalf("track %s samples=%d", tr.ID, tr.SampleCount())
		}
		for _, i := range []int{0, 17, 39} {
			s, err := tr.Index.At(i)
			if err != nil {
				t.Fatal(err)
			}
			if s.Offset != layout.SampleOffsets[ti][i] || s.Size != int64(layout.SampleSizes[ti][i]) {
				t.Fatalf("track %s sample %d off=%d want %d size=%d want %d",
					tr.ID, i, s.Offset, layout.SampleOffsets[ti][i], s.Size, layout.SampleSizes[ti][i])
			}
		}
	}
	// Only the even-numbered (video) generated tracks carry colour.
	for _, tr := range c.Tracks {
		got := tr.Color
		if tr.Type == "video" && (!got.Present || got.Primaries != 9 || got.Matrix != 9) {
			t.Fatalf("track %s color=%v", tr.ID, got)
		}
		if tr.Type == "audio" && got.Present {
			t.Fatalf("audio track %s unexpectedly has colour %v", tr.ID, got)
		}
	}
	if c.Fingerprint == "" {
		t.Fatal("empty fingerprint")
	}
}
