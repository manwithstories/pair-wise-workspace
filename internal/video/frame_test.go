package video

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"review/internal/index"
)

func sampleRows(n int) []index.PreviewRow {
	rows := make([]index.PreviewRow, n)
	for i := range rows {
		rows[i] = index.PreviewRow{
			Sample: uint32(i + 1), TimeScaled: uint32(i) * 1000,
			Keyframe: i%5 == 0,
		}
	}
	return rows
}

func TestExportDeterministicAndAtomic(t *testing.T) {
	dir := t.TempDir()
	exp, err := NewExporter(dir)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := exp.Export(sampleRows(12), 30000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 12 {
		t.Fatalf("frame count = %d", len(frames))
	}
	// No temp files survive a successful commit.
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", en.Name())
		}
	}
	first := SegmentHash(frames)
	// Rerun is idempotent: same hashes, no error, no duplicated content.
	again, err := exp.Export(sampleRows(12), 30000)
	if err != nil {
		t.Fatal(err)
	}
	if SegmentHash(again) != first {
		t.Fatal("segment hash changed across reruns")
	}
	// File content matches the recorded per-frame hash by construction.
	data, err := os.ReadFile(filepath.Join(dir, frames[0].FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sample=1") {
		t.Fatalf("frame content missing identity: %q", data)
	}
}

func TestExportRejectsBadPaths(t *testing.T) {
	for _, p := range []string{"relative/path", "/tmp/../etc/review", ""} {
		if _, err := NewExporter(p); err == nil {
			t.Fatalf("path %q accepted", p)
		}
	}
}

func TestExportEmptyInput(t *testing.T) {
	exp, err := NewExporter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exp.Export(nil, 30000); err == nil {
		t.Fatal("empty frame set must be rejected")
	}
}

func TestSegmentHashSensitiveToOrder(t *testing.T) {
	rows := sampleRows(6)
	dir := t.TempDir()
	exp, _ := NewExporter(dir)
	frames, err := exp.Export(rows, 30000)
	if err != nil {
		t.Fatal(err)
	}
	base := SegmentHash(frames)
	// Swap two frame descriptors; the segment digest must change.
	frames[1], frames[2] = frames[2], frames[1]
	if SegmentHash(frames) == base {
		t.Fatal("segment hash insensitive to frame order")
	}
}
