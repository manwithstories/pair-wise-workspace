package testkit_test

import (
	"os"
	"path/filepath"
	"testing"

	"archivearchivist/internal/model"
	"archivearchivist/internal/mp4"
	"archivearchivist/internal/rtp"
	"archivearchivist/internal/testkit"
)

func TestGenerateIsDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if _, err := testkit.Generate(testkit.Config{OutDir: a}); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.Generate(testkit.Config{OutDir: b}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"timeline.json", "stream.rtp", "video.mp4", "proxy.mp4"} {
		ab, err := os.ReadFile(filepath.Join(a, name))
		if err != nil {
			t.Fatal(err)
		}
		bb, err := os.ReadFile(filepath.Join(b, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(ab) != string(bb) {
			t.Fatalf("%s differs between runs", name)
		}
	}
}

func TestGeneratedTimelineValidates(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	tl, err := model.LoadTimeline(fx.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Tracks) != 4 {
		t.Fatalf("tracks = %d, want 4", len(tl.Tracks))
	}
}

func TestGeneratedFilesScan(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	byTrack := map[string]mp4.Scan{}
	for _, tm := range fx.Manifest.Tracks {
		sc, err := mp4.ScanFile(filepath.Join(dir, tm.File), tm.Track)
		if err != nil {
			t.Fatalf("%s: %v", tm.File, err)
		}
		if len(sc.Tracks) != 1 {
			t.Fatalf("%s: %d tracks", tm.File, len(sc.Tracks))
		}
		byTrack[tm.Track] = *sc
	}

	// V1 carries exactly one corrupt byte range at the planted offset.
	v1 := byTrack["V1"]
	if len(v1.CorruptRanges) != 1 {
		t.Fatalf("V1 corrupt ranges = %v", v1.CorruptRanges)
	}
	if v1.CorruptRanges[0].Start != fx.Manifest.CorruptByteStart {
		t.Fatalf("corrupt start = %d, want %d", v1.CorruptRanges[0].Start, fx.Manifest.CorruptByteStart)
	}

	// VFR proxy must not be reported as constant frame rate.
	if byTrack["P1"].Tracks[0].ConstantFrame {
		t.Fatalf("proxy reported constant frame rate")
	}
	if !byTrack["V1"].Tracks[0].ConstantFrame {
		t.Fatalf("video reported variable frame rate")
	}

	// The truncated copy is flagged without an error.
	if !byTrack["T1"].Truncated {
		t.Fatalf("truncated fixture not detected")
	}
	if !byTrack["T1"].Faults.Has(model.CodeTruncatedBox) {
		t.Fatalf("truncated fixture has no truncated_box fault: %v", byTrack["T1"].Faults)
	}
}

func TestGeneratedCaptureRecoversLoss(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	pkts, err := rtp.ReadCapture(fx.Capture)
	if err != nil {
		t.Fatal(err)
	}
	rep := rtp.Reorder(pkts, rtp.Options{Window: 64})
	if rep.Duplicates != fx.Manifest.DuplicateCount {
		t.Fatalf("duplicates = %d, want %d", rep.Duplicates, fx.Manifest.DuplicateCount)
	}
	if rep.Missing != len(fx.Manifest.LostSequences)+len(fx.Manifest.LateSequences) {
		t.Fatalf("missing = %d, want %d", rep.Missing, len(fx.Manifest.LostSequences)+len(fx.Manifest.LateSequences))
	}
	if rep.RecoveryRate().Cmp(model.NewRat(9, 10)) < 0 {
		t.Fatalf("recovery rate = %s, want >= 0.90", rep.RecoveryRate())
	}
	if fx.Manifest.TimestampWraps < 1 {
		t.Fatalf("fixture has no timestamp wrap")
	}
}
