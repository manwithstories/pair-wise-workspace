package mux

import (
	"context"
	"testing"

	"mediacore/internal/adapter"
	"mediacore/internal/media"
)

// TestOutputCarriesResolvedMetadata proves the muxer writes the resolved
// description into the delivered container and that it survives reparsing.
func TestOutputCarriesResolvedMetadata(t *testing.T) {
	layout, err := adapter.BuildMP4(adapter.MP4Spec{Tracks: 2, Samples: 12, Rotation: 1})
	if err != nil {
		t.Fatal(err)
	}
	src := adapter.NewLogicalSource(layout, "in.mp4")
	c, err := (&adapter.MP4Adapter{}).Parse(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	plan := map[string]uint32{}
	res := map[string]ResolvedMeta{}
	for i, tr := range c.Tracks {
		plan[tr.ID.String()] = uint32(i + 1)
		res[tr.ID.String()] = ResolvedMeta{
			Rotation: 2,
			Crop:     &media.Crop{X: 100, Y: 50, Width: 800, Height: 400},
			Color: media.Color{Present: true, Primaries: 9, Transfer: 16, Matrix: 9,
				FullRange: true, Source: "override"},
		}
	}
	outPath := t.TempDir() + "/o.mp4"
	r := &Remuxer{Tracks: c.Tracks, Plan: plan, Resolved: res, Source: src}
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
		t.Fatalf("reparse: %v", err)
	}
	for _, tr := range oc.Tracks {
		if tr.Rotation != 2 {
			t.Fatalf("track %s rotation=%d", tr.ID, tr.Rotation)
		}
		if !tr.Color.Present || tr.Color.Primaries != 9 || !tr.Color.FullRange {
			t.Fatalf("track %s color=%v", tr.ID, tr.Color)
		}
		if tr.Crop == nil || tr.Crop.Width != 800 || tr.Crop.Height != 400 {
			t.Fatalf("track %s crop=%v", tr.ID, tr.Crop)
		}
	}
}
