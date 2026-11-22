package batch

import (
	"errors"
	"path/filepath"
	"testing"

	"avscan/internal/model"
)

func summaryFor(path string, blockSize int) InputSummary {
	return InputSummary{
		Source:       path,
		FileSize:     1234,
		TrackCount:   2,
		SampleCounts: []int{100, 50},
		BlockSize:    blockSize,
	}
}

// deterministicProcessor counts key frames and faults from synthetic data.
func deterministicProcessor(trackID uint32, start, end int) (BlockResult, error) {
	res := BlockResult{TrackID: trackID, SampleStart: start, SampleEnd: end}
	for s := start; s < end; s++ {
		if s%12 == 0 {
			res.KeyFrames++
		}
	}
	return res, nil
}

func runAll(t *testing.T, path string, blockSize int, resume bool) (IndexRecord, *Runner) {
	t.Helper()
	summary := summaryFor(path, blockSize)
	r, err := NewRunner(RunOptions{
		IndexPath: path, BlockSize: blockSize, Resume: resume, Summary: summary,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if err := r.Process(1, 100, deterministicProcessor); err != nil {
		t.Fatalf("Process track1: %v", err)
	}
	if err := r.Process(2, 50, deterministicProcessor); err != nil {
		t.Fatalf("Process track2: %v", err)
	}
	rec := IndexRecord{InputDigest: r.cp.InputDigest, BlockSize: blockSize, Blocks: r.Blocks()}
	return rec, r
}

func TestFullRunAndChecksumStable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	rec1, r1 := runAll(t, path, 32, false)
	if r1.Completed() == 0 {
		t.Fatal("no blocks committed")
	}
	sum1, err := IndexSHA256(rec1)
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteJSON(path, rec1); err != nil {
		t.Fatal(err)
	}
	// Re-run from scratch must produce the identical index checksum.
	path2 := filepath.Join(dir, "index2.json")
	rec2, _ := runAll(t, path2, 32, false)
	sum2, _ := IndexSHA256(rec2)
	if sum1 != sum2 {
		t.Errorf("deterministic checksum mismatch:\n%s\n%s", sum1, sum2)
	}
}

func TestResumeAfterInterruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	blockSize := 32
	summary := summaryFor(path, blockSize)

	// First run: process only the first track, simulating an interruption.
	r, err := NewRunner(RunOptions{IndexPath: path, BlockSize: blockSize, Summary: summary})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Process(1, 100, deterministicProcessor); err != nil {
		t.Fatal(err)
	}
	partial := r.Completed()
	if partial == 0 {
		t.Fatal("expected some completed blocks before interruption")
	}

	// Full uninterrupted run for the reference checksum.
	full, _ := runAll(t, filepath.Join(dir, "full.json"), blockSize, false)
	wantSum, _ := IndexSHA256(full)

	// Resume and finish track 2.
	rec, r2 := runAll(t, path, blockSize, true)
	if !r2.Resumed() {
		t.Error("resume flag not detected")
	}
	gotSum, _ := IndexSHA256(rec)
	if gotSum != wantSum {
		t.Errorf("resumed index checksum differs from full run\n got %s\nwant %s", gotSum, wantSum)
	}
	if r2.Completed() != len(full.Blocks) {
		t.Errorf("completed = %d, want %d", r2.Completed(), len(full.Blocks))
	}
}

func TestResumeRejectsChangedInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	summary := summaryFor(path, 32)
	r, err := NewRunner(RunOptions{IndexPath: path, BlockSize: 32, Summary: summary})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Process(1, 100, deterministicProcessor); err != nil {
		t.Fatal(err)
	}
	changed := summary
	changed.FileSize = 9999
	if _, err := NewRunner(RunOptions{
		IndexPath: path, BlockSize: 32, Resume: true, Summary: changed,
	}); err == nil {
		t.Fatal("changed input must reject resume")
	} else if !errors.Is(err, model.ErrCheckpointMismatch) {
		t.Fatalf("err = %v, want ErrCheckpointMismatch", err)
	}
}

func TestDamagedBlockContinues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	r, err := NewRunner(RunOptions{
		IndexPath: path, BlockSize: 10, Summary: summaryFor(path, 10),
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	fn := func(trackID uint32, start, end int) (BlockResult, error) {
		calls++
		if start == 20 {
			return BlockResult{}, model.ErrDamagedInput
		}
		return BlockResult{TrackID: trackID, SampleStart: start, SampleEnd: end}, nil
	}
	if err := r.Process(1, 50, fn); err != nil {
		t.Fatalf("damaged block must not abort run: %v", err)
	}
	if calls != 5 {
		t.Errorf("processor calls = %d, want 5 (all blocks visited)", calls)
	}
	var damaged int
	for _, b := range r.Blocks() {
		if b.Damaged {
			damaged++
		}
	}
	if damaged != 1 {
		t.Errorf("damaged blocks = %d, want 1", damaged)
	}
}

func TestIndexWriteFailureKeepsOldCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	summary := summaryFor(path, 10)
	r, _ := NewRunner(RunOptions{IndexPath: path, BlockSize: 10, Summary: summary})
	if err := r.Process(1, 20, deterministicProcessor); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCheckpoint(path, summary)
	if err != nil || loaded == nil || loaded.Completed != 2 {
		t.Fatalf("checkpoint not established: %+v err=%v", loaded, err)
	}
	// Pointing at an unwritable directory must surface ErrIndexWrite and
	// leave the previous checkpoint intact.
	bad := filepath.Join(dir, "no_such_dir", "x.json")
	if err := SaveCheckpoint(bad, Checkpoint{InputDigest: "x"}); err == nil {
		t.Fatal("expected write failure")
	}
	loaded2, err := LoadCheckpoint(path, summary)
	if err != nil || loaded2.Completed != 2 {
		t.Fatalf("previous checkpoint lost: %+v err=%v", loaded2, err)
	}
}
