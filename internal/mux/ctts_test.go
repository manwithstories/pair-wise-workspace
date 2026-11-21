package mux

import (
	"context"
	"testing"

	"mediacore/internal/adapter"
	"mediacore/internal/media"
)

// TestCTTSRoundTripsPresentationOrder checks that non-zero composition
// offsets are written as ctts and read back unchanged.
func TestCTTSRoundTripsPresentationOrder(t *testing.T) {
	layout, err := adapter.BuildMP4(adapter.MP4Spec{Tracks: 1, Samples: 9, CTTS: true})
	if err != nil {
		t.Fatal(err)
	}
	src := adapter.NewLogicalSource(layout, "b.mp4")
	c, err := (&adapter.MP4Adapter{}).Parse(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	in := c.Tracks[0]
	// Capture the input presentation offsets.
	var want []int64
	for i := 0; i < in.SampleCount(); i++ {
		s, err := in.Index.At(i)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, media.RatToTicks(s.PTS.Sub(s.DTS), in.Timescale))
	}
	outPath := t.TempDir() + "/o.mp4"
	r := &Remuxer{Tracks: c.Tracks, Plan: map[string]uint32{"mp4:1": 1}, Source: src}
	if _, err := r.Run(context.Background(), Options{OutputPath: outPath}); err != nil {
		t.Fatal(err)
	}
	out, err := media.OpenFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	oc, err := (&adapter.MP4Adapter{}).Parse(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	ot := oc.Tracks[0]
	for i := range want {
		s, err := ot.Index.At(i)
		if err != nil {
			t.Fatal(err)
		}
		got := media.RatToTicks(s.PTS.Sub(s.DTS), ot.Timescale)
		if got != want[i] {
			t.Fatalf("sample %d presentation offset %d want %d", i, got, want[i])
		}
	}
}
