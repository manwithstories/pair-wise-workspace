package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pairflow/media/internal/clock"
	"pairflow/media/internal/mp4"
	"pairflow/media/internal/timeline"
)

func sampleTimeline(n int) *timeline.Timeline {
	var entries []timeline.Entry
	for i := 0; i < n; i++ {
		s := mp4.Sample{Number: i + 1, Offset: uint64(100 + i*8), Sync: i%4 == 0}
		end := clock.NewRational(int64(i+1), 25)
		entries = append(entries, timeline.Entry{
			Kind: timeline.KindVideo, Track: "video",
			Start: clock.NewRational(int64(i), 25), End: end,
			Frame: i + 1, Sample: &s, Offset: int64(s.Offset),
		})
	}
	return &timeline.Timeline{Entries: entries, FrameDuration: clock.NewRational(1, 25)}
}

func TestRenderProducesDeterministicFrames(t *testing.T) {
	tl := sampleTimeline(20)
	dir := t.TempDir()
	opt := Options{OutDir: dir, Width: 16, Height: 9, SegmentSize: 8, Seed: 41}
	targets := PlanTargets(tl, opt)
	if len(targets) != 20 {
		t.Fatalf("targets = %d", len(targets))
	}
	man, err := Render(tl, targets, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Frames) != 20 {
		t.Fatalf("frames = %d", len(man.Frames))
	}
	if len(man.Segments) != 3 {
		t.Fatalf("segments = %d, want 3", len(man.Segments))
	}
	if man.KeyframeHits != 5 {
		t.Fatalf("keyframe hits = %d", man.KeyframeHits)
	}
	// Every frame digest must match its file content.
	for _, f := range man.Frames {
		data, err := os.ReadFile(filepath.Join(dir, f.Path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			t.Fatalf("digest mismatch for %s", f.Path)
		}
	}
	// Re-render into a second directory: digests must be identical.
	dir2 := t.TempDir()
	opt2 := opt
	opt2.OutDir = dir2
	man2, err := Render(tl, PlanTargets(tl, opt2), opt2)
	if err != nil {
		t.Fatal(err)
	}
	if len(man2.Segments) != len(man.Segments) {
		t.Fatal("segment count differs across runs")
	}
	for i := range man.Segments {
		if man.Segments[i].SHA256 != man2.Segments[i].SHA256 {
			t.Fatalf("segment %d digest differs between runs", i)
		}
	}
	// Manifest must be valid JSON on disk.
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed Manifest
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if len(parsed.Frames) != 20 {
		t.Fatalf("reparsed frames = %d", len(parsed.Frames))
	}
}

func TestRenderRejectsPathTraversal(t *testing.T) {
	tl := sampleTimeline(2)
	_, err := Render(tl, PlanTargets(tl, Options{}), Options{OutDir: "../escape"})
	if err == nil {
		t.Fatal("expected traversal rejection")
	}
	se, ok := clock.AsSourceError(err)
	if !ok || se.Code != "path_traversal" {
		t.Fatalf("error = %v", err)
	}
	if _, err := safeDir(""); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestRenderRespectsFrameBudget(t *testing.T) {
	tl := sampleTimeline(30)
	dir := t.TempDir()
	opt := Options{OutDir: dir, Width: 32, Height: 32, SegmentSize: 4, MaxFrames: 10}
	man, err := Render(tl, PlanTargets(tl, opt), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Frames) != 10 {
		t.Fatalf("frames = %d", len(man.Frames))
	}
}

func TestRenderKeyframeOnly(t *testing.T) {
	tl := sampleTimeline(16)
	dir := t.TempDir()
	opt := Options{OutDir: dir, Width: 8, Height: 8, SegmentSize: 2, KeyframeOnly: true}
	man, err := Render(tl, PlanTargets(tl, opt), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Frames) != 4 {
		t.Fatalf("keyframe frames = %d", len(man.Frames))
	}
	for _, f := range man.Frames {
		if !f.Keyframe {
			t.Fatal("non-keyframe exported in keyframe-only mode")
		}
	}
}

func TestRasterizeIsDeterministicAndVaries(t *testing.T) {
	e := timeline.Entry{Kind: timeline.KindVideo, Start: clock.NewRational(1, 2), Offset: 42}
	a := rasterize(e, 3, Options{Width: 8, Height: 6, Seed: 41}, clock.NewRational(1, 25))
	b := rasterize(e, 3, Options{Width: 8, Height: 6, Seed: 41}, clock.NewRational(1, 25))
	c := rasterize(e, 4, Options{Width: 8, Height: 6, Seed: 41}, clock.NewRational(1, 25))
	if string(a) != string(b) {
		t.Fatal("rasterization is not deterministic")
	}
	if string(a) == string(c) {
		t.Fatal("different frames produced identical pixels")
	}
	wantHeader := "P6\n8 6\n255\n"
	if string(a[:len(wantHeader)]) != wantHeader {
		t.Fatalf("bad PPM header: %q", a[:len(wantHeader)])
	}
}
