package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"avpipe/internal/media"
)

// TestRunCancelStopsAndCleans verifies that a cancel budget reached by the
// feeder makes run return a cancel error promptly and leaves no output or
// temporary files behind.
func TestRunCancelStopsAndCleans(t *testing.T) {
	dir := t.TempDir()
	// The fixture set includes good.mp4, which the cancel budget can cut off
	// mid-stream.
	if _, err := generateFixtures(dir, 17); err != nil {
		t.Skipf("fixtures need ffmpeg: %v", err)
	}
	in := filepath.Join(dir, "good.mp4")
	out := filepath.Join(dir, "out.mp4")
	manifest := filepath.Join(dir, "m.json")

	start := time.Now()
	_, err := run(context.Background(), runConfig{
		Inputs: []string{in}, Output: out, Manifest: manifest, Format: "mp4",
		Segment: media.MustRat(2, 1), Workers: 4, Queue: 8, Cache: 8,
		WrapBits: 33, CancelAfter: 8,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected cancel error")
	}
	if !errors.Is(err, context.Canceled) && media.KindOf(err) != media.KindCancel {
		t.Fatalf("kind = %v (%v)", media.KindOf(err), err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("cancel took %v", elapsed)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("canceled run published output")
	}
	if _, statErr := os.Stat(manifest); !os.IsNotExist(statErr) {
		t.Fatal("canceled run published manifest")
	}
}

// TestRunRejectsCorruptWithoutPublishing verifies a corrupt input produces a
// corrupt error and no output.
func TestRunRejectsCorruptWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	if _, err := generateFixtures(dir, 17); err != nil {
		t.Skipf("fixtures need ffmpeg: %v", err)
	}
	out := filepath.Join(dir, "out.mp4")
	manifest := filepath.Join(dir, "m.json")
	_, err := run(context.Background(), runConfig{
		Inputs: []string{filepath.Join(dir, "badcrop.mp4")}, Output: out, Manifest: manifest,
		Format: "mp4", Segment: media.MustRat(2, 1), Workers: 2, Queue: 4, Cache: 4,
	})
	if err == nil {
		t.Fatal("expected corrupt error")
	}
	if media.KindOf(err) != media.KindCorrupt {
		t.Fatalf("kind = %v (%v)", media.KindOf(err), err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("corrupt run published output")
	}
}
