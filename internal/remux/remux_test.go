package remux_test

import (
	"path/filepath"
	"testing"

	"mp4replay/internal/fixture"
	"mp4replay/internal/framehash"
	"mp4replay/internal/mp4atom"
	"mp4replay/internal/remux"
	"mp4replay/internal/seekidx"
)

func TestExportRoundTrip(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.mp4")
	if err := fixture.Generate(fixture.Options{
		Path: srcPath, Tracks: 2, Frames: 400,
		Variant: fixture.VariantElstCttsWrap, MoovAtEnd: true,
	}); err != nil {
		t.Fatal(err)
	}
	src, err := mp4atom.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	vtr := src.VideoTrack()
	idx := seekidx.Build(vtr)
	land, err := idx.Locate(idx.PtsMs[121])
	if err != nil {
		t.Fatal(err)
	}
	start := land.Key.SampleIndex
	frames := 80
	end := start + frames
	spans := []remux.Span{{Track: vtr, Start: start, End: end}}
	if atr := src.AudioTrack(); atr != nil {
		spans = append(spans, remux.Span{Track: atr, Start: 0, End: 10})
	}
	outPath := filepath.Join(t.TempDir(), "clip.mp4")
	if err := remux.Export(src, spans, outPath); err != nil {
		t.Fatal(err)
	}
	clip, err := mp4atom.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer clip.Close()
	ctr := clip.VideoTrack()
	if !ctr.Samples[0].Sync {
		t.Fatal("clip must start on a sync sample")
	}
	if len(ctr.Samples) != frames {
		t.Fatalf("want %d clip samples, got %d", frames, len(ctr.Samples))
	}
	srcStart, err := framehash.FindSourceStart(src, clip, vtr, ctr)
	if err != nil {
		t.Fatal(err)
	}
	if srcStart != start {
		t.Fatalf("aligned at %d, want %d", srcStart, start)
	}
	matched, mismatched, err := framehash.CompareRange(
		src, clip, vtr, ctr, srcStart, 0, frames)
	if err != nil {
		t.Fatal(err)
	}
	if matched != frames || mismatched != 0 {
		t.Fatalf("matched=%d mismatched=%d", matched, mismatched)
	}
}

func TestExportRejectsNonSyncStart(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.mp4")
	fixture.Generate(fixture.Options{Path: srcPath, Frames: 100})
	src, _ := mp4atom.Open(srcPath)
	defer src.Close()
	tr := src.VideoTrack()
	// pick a non-sync sample
	nonSync := 1
	for tr.Samples[nonSync].Sync {
		nonSync++
	}
	err := remux.Export(src,
		[]remux.Span{{Track: tr, Start: nonSync, End: nonSync + 5}},
		filepath.Join(t.TempDir(), "x.mp4"))
	if err == nil {
		t.Fatal("expected rejection of non-sync export start")
	}
}
