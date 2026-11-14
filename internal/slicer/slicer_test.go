package slicer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tsprobe/internal/fixture"
)

func generateInput(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "sample.ts")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.Generate(file, fixture.Options{
		Programs: 2, Duration: 12 * time.Second, Loss: 7, CCJumps: 3, Versions: 4,
	})
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return path
}

func TestSliceAndVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	input := generateInput(t, dir)
	clips := filepath.Join(dir, "clips")
	index := filepath.Join(dir, "index.json")
	slices, err := Slice(input, clips, index, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(slices) < 3 || len(slices) > 6 {
		t.Fatalf("slices=%d, want 3..6", len(slices))
	}
	for _, item := range slices {
		info, err := os.Stat(filepath.Join(clips, item.File))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != item.ByteLength || item.ByteLength != int64(item.PacketCount)*188 {
			t.Fatalf("slice length mismatch: %+v size=%d", item, info.Size())
		}
		if len(item.PMTVersions) != 1 {
			t.Fatalf("slice PMT versions=%+v", item.PMTVersions)
		}
	}
	mismatches, details, err := Verify(index, clips)
	if err != nil {
		t.Fatal(err)
	}
	if mismatches != 0 {
		t.Fatalf("verify mismatches=%d: %v", mismatches, details)
	}
}

func TestShortTailMergesIntoPrevious(t *testing.T) {
	dir := t.TempDir()
	input := generateInput(t, dir)
	clips := filepath.Join(dir, "clips")
	index := filepath.Join(dir, "index.json")
	slices, err := Slice(input, clips, index, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(slices) != 1 {
		t.Fatalf("slices=%d, want short tail merged into one", len(slices))
	}
	if slices[0].DurationMS < 9000 {
		t.Fatalf("merged duration=%dms", slices[0].DurationMS)
	}
	mismatches, details, err := Verify(index, clips)
	if err != nil {
		t.Fatal(err)
	}
	if mismatches != 0 {
		t.Fatalf("merged verify mismatches=%d: %v", mismatches, details)
	}
}

func TestVerifyDetectsIndexMismatch(t *testing.T) {
	dir := t.TempDir()
	input := generateInput(t, dir)
	clips := filepath.Join(dir, "clips")
	indexPath := filepath.Join(dir, "index.json")
	if _, err := Slice(input, clips, indexPath, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	index, err := ReadIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	index.Slices[0].PacketCount++
	if err := WriteIndex(indexPath, index.Slices); err != nil {
		t.Fatal(err)
	}
	mismatches, _, err := Verify(indexPath, clips)
	if err != nil {
		t.Fatal(err)
	}
	if mismatches == 0 {
		t.Fatalf("tampered index was not detected")
	}
}

func TestMergeTailCopiesOnlyESPackets(t *testing.T) {
	dir := t.TempDir()
	previous := SliceInfo{File: "previous.ts", PacketCount: 2, ByteLength: 376, StartPTS: 0}
	tail := SliceInfo{
		File: "tail.ts", PacketCount: 4, ByteLength: 752, psiPackets: 1,
		StartPTS: 90000, DurationMS: 1000,
	}
	if err := os.WriteFile(filepath.Join(dir, previous.File), bytes.Repeat([]byte{0x47}, 376), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tail.File), bytes.Repeat([]byte{0x47}, 752), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mergeTail(dir, &previous, tail); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, previous.File))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 376+3*188 || previous.PacketCount != 5 || previous.ByteLength != info.Size() {
		t.Fatalf("merged metadata=%+v size=%d", previous, info.Size())
	}
	if _, err := os.Stat(filepath.Join(dir, tail.File)); !os.IsNotExist(err) {
		t.Fatalf("tail file was not removed: %v", err)
	}
}
