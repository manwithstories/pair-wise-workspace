package index

import (
	"errors"
	"path/filepath"
	"testing"
)

func sampleEntry(chunk, from, to int) Entry {
	return Entry{
		Chunk: chunk, FrameFrom: from, FrameTo: to,
		StartTick: int64(from) * 3000, EndTick: int64(to+1) * 3000,
		ValidFrames: to - from + 1, TotalFrames: to - from + 1, CorruptRanges: 0,
	}
}

func TestCommitAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "digest-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(sampleEntry(0, 0, 9)); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(sampleEntry(1, 10, 19)); err != nil {
		t.Fatal(err)
	}
	// duplicate chunk must not be counted again
	if err := s.Commit(sampleEntry(1, 10, 19)); !errors.Is(err, ErrChunkExists) {
		t.Fatalf("dup chunk err = %v, want ErrChunkExists", err)
	}
	sum := s.Summary()
	if sum.Chunks != 2 || sum.TotalFrames != 20 || sum.ValidFrames != 20 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.FirstTick != 0 || sum.LastTick != 60000 {
		t.Fatalf("time boundary = [%d,%d]", sum.FirstTick, sum.LastTick)
	}
	// reopen and confirm persisted state is rebuilt
	s2, err := Open(dir, "digest-1")
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Committed(0) || !s2.Committed(1) || s2.Committed(2) {
		t.Fatal("reopened store lost chunk state")
	}
	if got := s2.Summary().TotalFrames; got != 20 {
		t.Fatalf("reopened total = %d", got)
	}
}

func TestAtomicTempFileCleaned(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "d")
	if err := s.Commit(sampleEntry(0, 0, 4)); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, indexDirName, "*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}

func TestRejectInvalidEntryAndPath(t *testing.T) {
	s, _ := Open(t.TempDir(), "d")
	bad := sampleEntry(0, 10, 5) // inverted frame range
	if err := s.Commit(bad); err == nil {
		t.Fatal("inverted entry must be rejected")
	}
	if _, err := Open(t.TempDir()+"/../escape", "d"); err == nil {
		t.Fatal("path traversal must be rejected")
	}
}

func TestResetRebuild(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "d")
	_ = s.Commit(sampleEntry(0, 0, 9))
	_ = s.Commit(sampleEntry(1, 10, 19))
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if sum := s.Summary(); sum.Chunks != 0 || sum.TotalFrames != 0 {
		t.Fatalf("reset did not clear index: %+v", sum)
	}
	// store is reusable after reset
	if err := s.Commit(sampleEntry(0, 0, 9)); err != nil {
		t.Fatalf("commit after reset failed: %v", err)
	}
}
